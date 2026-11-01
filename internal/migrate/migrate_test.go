package migrate

import (
	"strings"
	"testing"
)

func TestRunRejectsBadCommands(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"no command", nil, "не указана команда"},
		{"unknown command", []string{"drop"}, "неизвестная команда"},
		{"up-to without version", []string{"up-to"}, "укажите номер версии"},
		{"down-to bad version", []string{"down-to", "x"}, "неверный номер версии"},
		{"down-to negative version", []string{"down-to", "-1"}, "неверный номер версии"},
		{"create without name", []string{"create"}, "укажите имя миграции"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Файла настроек нет: команда должна быть отвергнута раньше, чем он понадобится.
			err := Run("absent.ini", tt.args)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("got %v, want error with %q", err, tt.want)
			}
		})
	}
}

func TestCreateOutsideRepository(t *testing.T) {
	t.Chdir(t.TempDir())
	err := Run("absent.ini", []string{"create", "add_something"})
	if err == nil || !strings.Contains(err.Error(), "корня репозитория") {
		t.Fatalf("got %v", err)
	}
}
