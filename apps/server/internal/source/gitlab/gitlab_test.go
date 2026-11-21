package gitlab

import (
	"errors"
	"net/http"
	"testing"

	"git-event-hub/internal/normalize"
	"git-event-hub/internal/source"
	"git-event-hub/internal/testutil"
)

func TestVerify(t *testing.T) {
	src, err := New(source.Settings{ID: "gitlab", Secret: "tok-a, tok-b"})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name, token string
		want        error
	}{
		{"first token", "tok-a", nil},
		{"second token", "tok-b", nil},
		{"wrong", "tok-c", source.ErrBadSignature},
		{"prefix of a token", "tok-", source.ErrBadSignature},
		{"empty", "", source.ErrNoSignature},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := http.Header{}
			if tt.token != "" {
				h.Set("X-Gitlab-Token", tt.token)
			}
			if err := src.Verify(&source.Request{Header: h}); !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestNewRefusesMissingSecret(t *testing.T) {
	if _, err := New(source.Settings{ID: "gitlab"}); !errors.Is(err, source.ErrNoSecret) {
		t.Errorf("no secret: err = %v", err)
	}
	if _, err := New(source.Settings{ID: "gitlab", Secret: " , "}); !errors.Is(err, source.ErrNoSecret) {
		t.Error("blank tokens were accepted")
	}
}

func TestDeliveryID(t *testing.T) {
	src, _ := New(source.Settings{ID: "gitlab", Secret: "t"})
	h := http.Header{}
	h.Set("X-Gitlab-Event-UUID", "uuid-1")
	if id, _ := src.DeliveryID(&source.Request{Header: h}); id != "uuid-1" {
		t.Errorf("id = %q", id)
	}
	h.Set("Idempotency-Key", "key-1")
	if id, _ := src.DeliveryID(&source.Request{Header: h}); id != "key-1" {
		t.Errorf("id = %q, want the idempotency key first", id)
	}
	a, _ := src.DeliveryID(&source.Request{Header: http.Header{}, Body: []byte("one")})
	b, _ := src.DeliveryID(&source.Request{Header: http.Header{}, Body: []byte("one")})
	c, _ := src.DeliveryID(&source.Request{Header: http.Header{}, Body: []byte("two")})
	if a != b || a == c {
		t.Errorf("body ids: %q %q %q", a, b, c)
	}
}

func TestParse(t *testing.T) {
	src, _ := New(source.Settings{ID: "gitlab", Secret: "t"})
	r := &source.Request{Header: http.Header{}, Body: testutil.Fixture(t, "gitlab/merge_request.open.json")}
	events, err := src.Parse(r, normalize.Context{DeliveryID: "u1"})
	if err != nil || len(events) != 1 || events[0].ID != "gitlab:u1:0" {
		t.Fatalf("events = %+v err = %v", events, err)
	}
}
