package worker

import (
	"errors"
	"strings"
	"testing"
)

func TestUserinfo(t *testing.T) {
	tests := []struct {
		name string
		url  string
		want string
	}{
		{"simple", "amqp://user:pass@host:5672/", "user:pass"},
		{"password contains @", "amqp://user:p@ss@host:5672/", "user:p@ss"},
		{"no credentials", "amqp://host:5672/", ""},
		{"no scheme", "not-a-url", ""},
		{"no vhost path", "amqp://user:pass@host:5672", "user:pass"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := userinfo(tt.url); got != tt.want {
				t.Errorf("userinfo(%q) = %q, want %q", tt.url, got, tt.want)
			}
		})
	}
}

func TestRedactDialErr(t *testing.T) {
	rawURL := `amqp://user:p%ssword@rabbitmq.svc.cluster.local:5672/`
	underlying := errors.New(`parse "` + rawURL + `": invalid URL escape "%ss"`)

	err := redactDialErr(rawURL, underlying)

	if strings.Contains(err.Error(), "p%ssword") {
		t.Fatalf("redactDialErr leaked the credential: %v", err)
	}
	if !strings.Contains(err.Error(), "***") {
		t.Fatalf("redactDialErr did not redact anything: %v", err)
	}
}

func TestRedactDialErrNoCredentials(t *testing.T) {
	underlying := errors.New("connection refused")
	err := redactDialErr("amqp://host:5672/", underlying)

	if err.Error() != underlying.Error() {
		t.Fatalf("expected error unchanged when URL has no credentials, got %q", err.Error())
	}
}
