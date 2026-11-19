package pi

import (
	"context"
	"errors"
	"testing"
	"time"
)

func loginEngine(t *testing.T) *Engine {
	t.Helper()
	c := fakePiCommand(t, "rpc")
	e := New(Config{Command: c.Path, Env: c.Env, StopGrace: 200 * time.Millisecond})
	t.Cleanup(e.Close)
	return e
}

// waitLogin waits until check accepts the login state.
func waitLogin(t *testing.T, l *Login, check func(LoginState) bool) LoginState {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		s, changed := l.State()
		if check(s) {
			return s
		}
		select {
		case <-changed:
		case <-deadline:
			t.Fatalf("login state %+v", s)
		}
	}
}

func TestLoginOverRPC(t *testing.T) {
	for _, tc := range []struct{ code, want string }{{"good\n", "done"}, {"bad", "failed"}} {
		l, err := loginEngine(t).Login(context.Background(), "/ext/pi-auth.mjs")
		if err != nil {
			t.Fatal(err)
		}
		s := waitLogin(t, l, func(s LoginState) bool { return s.Prompt != nil })
		if *s.Prompt != "模型提供方" || len(s.Options) != 1 {
			t.Fatalf("provider prompt %+v", s)
		}
		if err := l.Answer(context.Background(), "openai"); err == nil {
			t.Fatal("option outside the list accepted")
		}
		_, changed := l.State()
		if err := l.Answer(context.Background(), "anthropic"); err != nil {
			t.Fatal(err)
		}
		select {
		case <-changed:
		default:
			t.Fatal("Answer did not signal the cleared prompt")
		}
		s = waitLogin(t, l, func(s LoginState) bool { return s.Prompt != nil && *s.Prompt == "Code" })
		if s.URL == nil || *s.URL != "https://auth.example/login?x=1" || !s.Secret || *s.Instructions != "Open the link" {
			t.Fatalf("link state %+v", s)
		}
		if err := l.Answer(context.Background(), "a\nb"); err == nil {
			t.Fatal("multi-line code accepted")
		}
		if err := l.Answer(context.Background(), tc.code); err != nil {
			t.Fatal(err)
		}
		<-l.Done()
		if s, _ := l.State(); s.State != tc.want {
			t.Fatalf("code %q ended %q", tc.code, s.State)
		}
		select {
		case <-l.proc.Done():
		default:
			t.Fatal("login Pi still running")
		}
		if err := l.Answer(context.Background(), "x"); !errors.Is(err, ErrLoginOver) {
			t.Fatalf("answer after the end: %v", err)
		}
	}
}

func TestLoginCancelAndMissingExtension(t *testing.T) {
	e := loginEngine(t)
	l, err := e.Login(context.Background(), "/ext/pi-auth.mjs")
	if err != nil {
		t.Fatal(err)
	}
	waitLogin(t, l, func(s LoginState) bool { return s.Prompt != nil })
	l.Cancel()
	if s, _ := l.State(); s.State != "failed" {
		t.Fatalf("cancelled login %q", s.State)
	}
	<-l.proc.Done()

	l, err = e.Login(context.Background(), "/ext/pi-auth.mjs")
	if err != nil {
		t.Fatal(err)
	}
	waitLogin(t, l, func(s LoginState) bool { return s.Prompt != nil })
	e.Close()
	select {
	case <-l.proc.Done():
	default:
		t.Fatal("Engine.Close left a login Pi running")
	}

	c := fakePiCommand(t, "rpc")
	e = New(Config{Command: c.Path, Env: append(c.Env, "PERSONA_TEST_NO_AUTH=1")})
	t.Cleanup(e.Close)
	if _, err := e.Login(context.Background(), "/ext/pi-auth.mjs"); err == nil {
		t.Fatal("Pi without /persona-auth accepted")
	}
}
