// Package delivery — асинхронная доставка сообщений из очереди в PostgreSQL в Telegram
// с ретраями и соблюдением лимитов Telegram.
package delivery

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/time/rate"

	"github.com/blind-postman/blind-postman/internal/storage"
	"github.com/blind-postman/blind-postman/internal/tgbot"
)

type Store interface {
	ClaimMessages(ctx context.Context, limit int, lease, chatInterval time.Duration) ([]storage.ClaimedMessage, error)
	CompleteMessage(ctx context.Context, m storage.ClaimedMessage, tgMessageID int) error
	FinishMessage(ctx context.Context, id int64, status, reason string) error
	RetryMessage(ctx context.Context, id int64, delay time.Duration, reason string, countAttempt bool) error
	ExpireMessages(ctx context.Context) (int64, error)
	SetStatus(ctx context.Context, subscriberID int64, status, reason string) error
	ListenMessages(ctx context.Context, notify func()) error
}

type Sender interface {
	Send(ctx context.Context, telegramID int64, text string, o tgbot.SendOptions) (int, error)
}

type Config struct {
	// Workers — сколько сообщений отправляется одновременно.
	Workers int
	// GlobalRate — общий лимит сообщений в секунду (Telegram: ~30/с на бота).
	GlobalRate float64
	// ChatInterval — минимальный интервал между сообщениями в один чат (Telegram: ~1/с).
	ChatInterval time.Duration
	// MaxAttempts — после стольких неудачных попыток сообщение считается недоставленным.
	MaxAttempts int
	// PollInterval — как часто проверять очередь без уведомлений (ретраи, интервал чата).
	PollInterval time.Duration

	// Lease — аренда сообщения воркером; должна быть больше LimiterWait + SendTimeout.
	Lease       time.Duration
	LimiterWait time.Duration
	SendTimeout time.Duration
	BaseBackoff time.Duration
	MaxBackoff  time.Duration
}

func (c Config) withDefaults() Config {
	def := func(v *time.Duration, d time.Duration) {
		if *v <= 0 {
			*v = d
		}
	}
	if c.Workers <= 0 {
		c.Workers = 8
	}
	if c.GlobalRate <= 0 {
		c.GlobalRate = 25
	}
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = 10
	}
	def(&c.ChatInterval, time.Second)
	def(&c.PollInterval, time.Second)
	def(&c.Lease, time.Minute)
	def(&c.LimiterWait, 10*time.Second)
	def(&c.SendTimeout, 20*time.Second)
	def(&c.BaseBackoff, 2*time.Second)
	def(&c.MaxBackoff, 5*time.Minute)
	return c
}

type Dispatcher struct {
	cfg     Config
	store   Store
	sender  Sender
	limiter *rate.Limiter
	// pausedUntil (unix nano) — после 429 от Telegram не отправляем ничего до этого момента.
	pausedUntil atomic.Int64
	wake        chan struct{}
	log         *slog.Logger
}

func New(cfg Config, store Store, sender Sender, log *slog.Logger) *Dispatcher {
	cfg = cfg.withDefaults()
	return &Dispatcher{
		cfg:     cfg,
		store:   store,
		sender:  sender,
		limiter: rate.NewLimiter(rate.Limit(cfg.GlobalRate), max(1, int(cfg.GlobalRate))),
		wake:    make(chan struct{}, 1),
		log:     log.With("component", "delivery"),
	}
}

// Wake будит диспетчер, не блокируясь.
func (d *Dispatcher) Wake() {
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

// Run обрабатывает очередь до отмены ctx, затем дожидается сообщений, находящихся в отправке.
func (d *Dispatcher) Run(ctx context.Context) {
	d.log.Info("dispatcher started", "workers", d.cfg.Workers, "global_rate", d.cfg.GlobalRate,
		"chat_interval", d.cfg.ChatInterval)
	go d.listen(ctx)

	slots := make(chan struct{}, d.cfg.Workers)
	var wg sync.WaitGroup
	defer wg.Wait()

	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-d.wake:
		case <-timer.C:
		}

		wait := d.cfg.PollInterval
		if p := d.pauseLeft(); p > 0 {
			wait = p
		} else if free := cap(slots) - len(slots); free > 0 {
			d.expire(ctx)
			msgs, err := d.store.ClaimMessages(ctx, free, d.cfg.Lease, d.cfg.ChatInterval)
			if err != nil && ctx.Err() == nil {
				d.log.Error("claim messages", "err", err)
			}
			for _, m := range msgs {
				slots <- struct{}{}
				wg.Add(1)
				go func() {
					defer func() {
						<-slots
						wg.Done()
						d.Wake() // освободился слот — возможно, в очереди есть ещё
					}()
					d.deliver(ctx, m)
				}()
			}
		}
		timer.Reset(wait)
	}
}

func (d *Dispatcher) listen(ctx context.Context) {
	for {
		err := d.store.ListenMessages(ctx, d.Wake)
		if ctx.Err() != nil {
			return
		}
		d.log.Warn("listen for new messages failed, falling back to polling", "err", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
}

func (d *Dispatcher) expire(ctx context.Context) {
	n, err := d.store.ExpireMessages(ctx)
	if err != nil && ctx.Err() == nil {
		d.log.Error("expire messages", "err", err)
	}
	if n > 0 {
		d.log.Warn("messages expired undelivered", "count", n)
	}
}

func (d *Dispatcher) deliver(ctx context.Context, m storage.ClaimedMessage) {
	log := d.log.With("message_id", m.ID, "telegram_id", m.TelegramID, "attempt", m.Attempts)
	// Результат фиксируем и при остановке сервиса, иначе сообщение будет ждать конца аренды.
	dbCtx := context.WithoutCancel(ctx)

	switch {
	case m.SubscriberStatus != storage.StatusActive:
		d.finish(dbCtx, log, m, storage.MessageFailed, "subscriber is "+m.SubscriberStatus)
		return
	case m.ExpiresAt != nil && !time.Now().Before(*m.ExpiresAt):
		d.finish(dbCtx, log, m, storage.MessageExpired, "")
		return
	}

	if err := d.waitTurn(ctx); err != nil {
		// Не дождались очереди лимитера (остановка, пауза после 429) — вернём без штрафа.
		d.retry(dbCtx, log, m, d.pauseLeft(), "", false)
		return
	}

	sendCtx, cancel := context.WithTimeout(dbCtx, d.cfg.SendTimeout)
	tgID, err := d.sender.Send(sendCtx, m.TelegramID, m.Text, tgbot.SendOptions{
		ParseMode:           m.ParseMode,
		DisableNotification: m.DisableNotification,
	})
	cancel()

	var rl *tgbot.RateLimitError
	switch {
	case err == nil:
		if err := d.store.CompleteMessage(dbCtx, m, tgID); err != nil {
			log.Error("mark message sent", "err", err)
		}
	case errors.Is(err, tgbot.ErrForbidden):
		d.finish(dbCtx, log, m, storage.MessageFailed, err.Error())
		if err := d.store.SetStatus(dbCtx, m.SubscriberID, storage.StatusBlocked, "telegram forbidden"); err != nil {
			log.Error("mark subscriber blocked", "err", err)
		}
	case errors.As(err, &rl):
		d.pause(rl.RetryAfter)
		log.Warn("telegram rate limit, pausing", "retry_after", rl.RetryAfter)
		d.retry(dbCtx, log, m, rl.RetryAfter, err.Error(), false)
	case errors.Is(err, tgbot.ErrRejected):
		d.finish(dbCtx, log, m, storage.MessageFailed, err.Error())
	case m.Attempts >= d.cfg.MaxAttempts:
		d.finish(dbCtx, log, m, storage.MessageFailed, "max attempts reached: "+err.Error())
	default:
		delay := backoff(m.Attempts, d.cfg.BaseBackoff, d.cfg.MaxBackoff)
		log.Warn("send failed, will retry", "err", err, "delay", delay)
		d.retry(dbCtx, log, m, delay, err.Error(), true)
	}
}

// waitTurn ждёт разрешения глобального лимитера, но не дольше LimiterWait,
// чтобы не пересидеть аренду сообщения.
func (d *Dispatcher) waitTurn(ctx context.Context) error {
	if d.pauseLeft() > 0 {
		return errPaused
	}
	ctx, cancel := context.WithTimeout(ctx, d.cfg.LimiterWait)
	defer cancel()
	if err := d.limiter.Wait(ctx); err != nil {
		return err
	}
	if d.pauseLeft() > 0 { // пауза могла начаться, пока ждали
		return errPaused
	}
	return nil
}

var errPaused = errors.New("delivery: paused by telegram rate limit")

func (d *Dispatcher) pause(dur time.Duration) {
	until := time.Now().Add(dur).UnixNano()
	for {
		cur := d.pausedUntil.Load()
		if cur >= until || d.pausedUntil.CompareAndSwap(cur, until) {
			return
		}
	}
}

func (d *Dispatcher) pauseLeft() time.Duration {
	return max(0, time.Until(time.Unix(0, d.pausedUntil.Load())))
}

func (d *Dispatcher) finish(ctx context.Context, log *slog.Logger, m storage.ClaimedMessage, status, reason string) {
	log.Warn("message not delivered", "status", status, "reason", reason)
	if err := d.store.FinishMessage(ctx, m.ID, status, reason); err != nil {
		log.Error("finish message", "err", err)
	}
}

func (d *Dispatcher) retry(ctx context.Context, log *slog.Logger, m storage.ClaimedMessage, delay time.Duration, reason string, countAttempt bool) {
	if err := d.store.RetryMessage(ctx, m.ID, delay, reason, countAttempt); err != nil {
		log.Error("reschedule message", "err", err)
	}
}

// backoff — экспоненциальная задержка base·2^(attempt-1) с потолком max и джиттером ±20%.
func backoff(attempt int, base, maxDelay time.Duration) time.Duration {
	d := maxDelay
	if shift := attempt - 1; shift < 30 {
		d = min(base<<max(0, shift), maxDelay)
	}
	jitter := 0.8 + 0.4*rand.Float64()
	return time.Duration(float64(d) * jitter)
}
