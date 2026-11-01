package config

import (
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/blind-postman/blind-postman/pkg/winnow"
)

const testSecret = "0123456789abcdef0123456789abcdef"

func validINI(password string) string {
	return `[config]
listen=127.0.0.1:9000
api_token=0123456789abcdef

[telegram]
bot_token=123:abc
developer_id=5000000000
global_rate=20.5
chat_interval=2s

[database]
host = "db.example.com"
port = 35437
name = "postman"
user = "postman"
password = "` + password + `"

[connect]
link_shared_secret=` + testSecret + `
link_ttl=2h

[alerts]
message_ttl=30m
message_retention=720h

[send-queue]
queue_workers=4
queue_max_attempts=7
queue_poll_interval=500ms
`
}

func load(t *testing.T, content string) (Config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.ini")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return Load(path)
}

func TestLoad(t *testing.T) {
	password := `p@ss/w:rd?#%`
	c, err := load(t, validINI(winnow.Tangle(password)))
	if err != nil {
		t.Fatal(err)
	}

	if c.HTTPAddr != "127.0.0.1:9000" || c.APIKey != "0123456789abcdef" || c.TelegramToken != "123:abc" ||
		string(c.SharedSecret) != testSecret {
		t.Errorf("unexpected config: %+v", c)
	}
	if c.LinkTTL != 2*time.Hour || c.MessageTTL != 30*time.Minute || c.MessageRetention != 720*time.Hour {
		t.Errorf("ttl: link %v, message %v, retention %v", c.LinkTTL, c.MessageTTL, c.MessageRetention)
	}
	if c.DeveloperID != 5000000000 {
		t.Errorf("developer id %d", c.DeveloperID)
	}
	want := Queue{Workers: 4, MaxAttempts: 7, GlobalRate: 20.5, ChatInterval: 2 * time.Second, PollInterval: 500 * time.Millisecond}
	if c.Queue != want {
		t.Errorf("queue %+v, want %+v", c.Queue, want)
	}

	u, err := url.Parse(c.DatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := u.User.Password()
	if got != password || u.User.Username() != "postman" || u.Host != "db.example.com:35437" || u.Path != "/postman" {
		t.Errorf("database url parsed to user %q host %q path %q, password match %v",
			u.User.Username(), u.Host, u.Path, got == password)
	}
}

func TestLoadDefaults(t *testing.T) {
	c, err := load(t, `[config]
api_token=0123456789abcdef
[telegram]
bot_token=123:abc
[database]
host=localhost
name=postman
user=postman
password=plain
sslmode=disable
[connect]
link_shared_secret=`+testSecret+`
`)
	if err != nil {
		t.Fatal(err)
	}
	if c.HTTPAddr != ":8080" || c.LinkTTL != 24*time.Hour || c.MessageTTL != time.Hour || c.MessageRetention != 0 ||
		c.CallbackTTL != 10*time.Minute || c.CallbackURL != "" || c.DeveloperID != 0 {
		t.Errorf("unexpected defaults: %+v", c)
	}
	if want := "postgres://postman:plain@localhost:5432/postman?sslmode=disable"; c.DatabaseURL != want {
		t.Errorf("got %q, want %q", c.DatabaseURL, want)
	}
	if c.Queue != (Queue{Workers: 8, MaxAttempts: 10, GlobalRate: 25, ChatInterval: time.Second, PollInterval: time.Second}) {
		t.Errorf("queue defaults: %+v", c.Queue)
	}
}

func TestMessageTTLZeroMeansNoExpiry(t *testing.T) {
	c, err := load(t, strings.Replace(validINI("plain"), "message_ttl=30m", "message_ttl=0", 1))
	if err != nil {
		t.Fatal(err)
	}
	if c.MessageTTL != 0 {
		t.Errorf("got %v, want 0", c.MessageTTL)
	}
}

func TestMessageRetentionZeroKeepsMessages(t *testing.T) {
	c, err := load(t, strings.Replace(validINI("plain"), "message_retention=720h", "message_retention=0", 1))
	if err != nil {
		t.Fatal(err)
	}
	if c.MessageRetention != 0 {
		t.Errorf("got %v, want 0", c.MessageRetention)
	}
}

func TestLoadErrors(t *testing.T) {
	bad := "{" + strings.Repeat("A", 40) + "}"
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{"missing token", strings.Replace(validINI("x"), "bot_token=123:abc", "", 1), "[telegram] bot_token is required"},
		{"short secret", strings.Replace(validINI("x"), testSecret, "short", 1), "link_shared_secret must be at least"},
		{"short api token", strings.Replace(validINI("x"), "api_token=0123456789abcdef", "api_token=short", 1), "api_token must be at least"},
		{"bad duration", strings.Replace(validINI("x"), "link_ttl=2h", "link_ttl=soon", 1), "[connect] link_ttl"},
		{"bad workers", strings.Replace(validINI("x"), "queue_workers=4", "queue_workers=-1", 1), "[send-queue] queue_workers"},
		{"bad developer id", strings.Replace(validINI("x"), "developer_id=5000000000", "developer_id=dima", 1), "[telegram] developer_id"},
		{"bad port", strings.Replace(validINI("x"), "port = 35437", "port = 70000", 1), "port out of range"},
		{"bad password", validINI(bad), "password: cannot decrypt"},
		{"not ini", "just some text", "line 1"},
		{"key outside section", "key=value", "outside of a section"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := load(t, tt.content)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("got %v, want error with %q", err, tt.want)
			}
			if strings.Contains(err.Error(), bad) {
				t.Fatal("error must not contain the password")
			}
		})
	}
}

func TestExampleIsValid(t *testing.T) {
	if _, err := Load("../../config.example.ini"); err != nil {
		t.Fatal(err)
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "absent.ini")); err == nil {
		t.Fatal("expected error")
	}
}

func TestParseINI(t *testing.T) {
	ini, err := parseINI([]byte("\xEF\xBB\xBF; comment\r\n# comment\r\n[Send-Queue]\r\n" +
		"Key = value with spaces \r\nsemi = a;b#c\r\nquoted = \" keeps spaces \"\r\neq = a=b\r\nlone = \"\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"key":    "value with spaces",
		"semi":   "a;b#c",
		"quoted": " keeps spaces ",
		"eq":     "a=b",
		"lone":   `"`,
	}
	for k, v := range want {
		if got := ini["send-queue"][k]; got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
}
