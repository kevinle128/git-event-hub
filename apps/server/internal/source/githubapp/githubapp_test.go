package githubapp

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"testing"

	"git-event-hub/internal/normalize"
	"git-event-hub/internal/source"
	"git-event-hub/internal/testutil"
)

func sign(secret string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

func TestVerify(t *testing.T) {
	src, err := New(source.Settings{ID: "github", Secret: "s3cret"})
	if err != nil {
		t.Fatal(err)
	}
	body := testutil.Fixture(t, "github/pull_request.opened.json")
	tests := []struct {
		name   string
		header string
		body   []byte
		want   error
	}{
		{"good", sign("s3cret", body), body, nil},
		{"wrong secret", sign("other", body), body, source.ErrBadSignature},
		{"body changed", sign("s3cret", body), append([]byte("x"), body...), source.ErrBadSignature},
		{"missing", "", body, source.ErrNoSignature},
		{"no prefix", hex.EncodeToString([]byte("abc")), body, source.ErrBadSignature},
		{"not hex", "sha256=zzzz", body, source.ErrBadSignature},
		{"sha1 style header is not accepted", "sha1=" + hex.EncodeToString([]byte("abc")), body, source.ErrBadSignature},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := http.Header{}
			if tt.header != "" {
				h.Set("X-Hub-Signature-256", tt.header)
			}
			err := src.Verify(&source.Request{Header: h, Body: tt.body})
			if !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestNeedsSecret(t *testing.T) {
	if _, err := New(source.Settings{ID: "github"}); !errors.Is(err, source.ErrNoSecret) {
		t.Fatalf("err = %v", err)
	}
}

func TestDeliveryIDAndParse(t *testing.T) {
	src, _ := New(source.Settings{ID: "github", Secret: "s"})
	h := http.Header{}
	if _, err := src.DeliveryID(&source.Request{Header: h}); !errors.Is(err, source.ErrNoDeliveryID) {
		t.Fatalf("err = %v", err)
	}
	h.Set("X-GitHub-Delivery", "72d3162e-cc78-11e3-81ab-4c9367dc0958")
	h.Set("X-GitHub-Event", "pull_request")
	r := &source.Request{Header: h, Body: testutil.Fixture(t, "github/pull_request.opened.json")}
	id, err := src.DeliveryID(r)
	if err != nil || id != "72d3162e-cc78-11e3-81ab-4c9367dc0958" {
		t.Fatalf("id = %q err = %v", id, err)
	}
	events, err := src.Parse(r, normalize.Context{DeliveryID: id})
	if err != nil || len(events) != 1 || events[0].ID != "github:72d3162e-cc78-11e3-81ab-4c9367dc0958:0" {
		t.Fatalf("events = %+v err = %v", events, err)
	}
}
