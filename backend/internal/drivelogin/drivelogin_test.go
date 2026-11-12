package drivelogin

import (
	"context"
	"errors"
	"testing"
	"time"
)

func fixedProvider(statuses ...Status) Provider {
	return func(context.Context) (Challenge, error) {
		next := 0
		return Challenge{
			QRImageDataURL: "data:image/png;base64,AA==",
			Interval:       1500 * time.Millisecond,
			Poll: func(context.Context) (Status, error) {
				status := statuses[next]
				next++
				return status, nil
			},
		}, nil
	}
}

func TestLoginReportsProgressThenCredentialsAndEnds(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// fixedProvider panics if polled a third time, so repeats must not reach it.
	m := New(map[string]Provider{"demo": fixedProvider(
		Status{State: Scanned, Interval: 5 * time.Second},
		Status{State: Success, Credentials: map[string]string{"access_token": "token", "family_id": ""}},
	)})
	m.now = func() time.Time { return now }
	session, err := m.Start(context.Background(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	if session.ID == "" || session.Kind != "demo" || session.IntervalMs != 1500 || session.ExpiresAt.IsZero() {
		t.Fatalf("session = %#v", session)
	}
	first, err := m.Poll(context.Background(), session.ID)
	if err != nil || first.State != Scanned || first.IntervalMs != 5000 || first.Credentials != nil {
		t.Fatalf("first = %#v, %v", first, err)
	}
	second, err := m.Poll(context.Background(), session.ID)
	if err != nil || second.State != Success || len(second.Credentials) != 1 || second.Credentials["access_token"] != "token" {
		t.Fatalf("second = %#v, %v", second, err)
	}
	// A client that lost the success response can poll again.
	now = now.Add(FinishedRetention - time.Second)
	repeated, err := m.Poll(context.Background(), session.ID)
	if err != nil || repeated.State != Success || repeated.Credentials["access_token"] != "token" {
		t.Fatalf("repeated poll = %#v, %v", repeated, err)
	}
	now = now.Add(2 * time.Second)
	if _, err := m.Poll(context.Background(), session.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("poll after retention: %v", err)
	}
}

func TestCanceledFinishedLoginDropsCredentials(t *testing.T) {
	m := New(map[string]Provider{"demo": fixedProvider(Status{State: Success, Credentials: map[string]string{"cookie": "c"}})})
	session, _ := m.Start(context.Background(), "demo")
	if result, err := m.Poll(context.Background(), session.ID); err != nil || result.State != Success {
		t.Fatalf("result = %#v, %v", result, err)
	}
	m.Cancel(session.ID)
	if _, err := m.Poll(context.Background(), session.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("poll after cancel: %v", err)
	}
}

func TestFinishedLoginsArePrunedAfterRetention(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	m := New(map[string]Provider{"demo": fixedProvider(Status{State: Rejected})})
	m.now = func() time.Time { return now }
	finished, _ := m.Start(context.Background(), "demo")
	if result, _ := m.Poll(context.Background(), finished.ID); result.State != Rejected {
		t.Fatalf("result = %#v", result)
	}
	now = now.Add(FinishedRetention + time.Second)
	if _, err := m.Start(context.Background(), "demo"); err != nil {
		t.Fatal(err)
	}
	if _, kept := m.sessions[finished.ID]; kept {
		t.Fatal("finished login outlived its retention")
	}
}

func TestSuccessWithoutCredentialsFails(t *testing.T) {
	m := New(map[string]Provider{"demo": fixedProvider(Status{State: Success, Credentials: map[string]string{"cookie": ""}})})
	session, _ := m.Start(context.Background(), "demo")
	result, err := m.Poll(context.Background(), session.ID)
	if err != nil || result.State != Failed || result.Message == "" || result.Credentials != nil {
		t.Fatalf("result = %#v, %v", result, err)
	}
}

func TestExpiredLoginStopsWithoutCallingProvider(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	m := New(map[string]Provider{"demo": func(context.Context) (Challenge, error) {
		return Challenge{
			QRImageDataURL: "data:image/png;base64,AA==",
			ExpiresAt:      now.Add(time.Minute),
			Poll: func(context.Context) (Status, error) {
				t.Fatal("expired session polled the provider")
				return Status{}, nil
			},
		}, nil
	}})
	m.now = func() time.Time { return now }
	session, _ := m.Start(context.Background(), "demo")
	if session.IntervalMs != DefaultInterval.Milliseconds() {
		t.Fatalf("interval = %d", session.IntervalMs)
	}
	now = now.Add(2 * time.Minute)
	for range 2 {
		result, err := m.Poll(context.Background(), session.ID)
		if err != nil || result.State != Expired {
			t.Fatalf("result = %#v, %v", result, err)
		}
	}
}

func TestUnknownKindsSessionsAndProviderErrors(t *testing.T) {
	m := New(map[string]Provider{"broken": func(context.Context) (Challenge, error) {
		return Challenge{}, errors.New("upstream down")
	}})
	if _, err := m.Start(context.Background(), "other"); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("unsupported kind: %v", err)
	}
	if _, err := m.Start(context.Background(), "broken"); err == nil || err.Error() != "upstream down" {
		t.Fatalf("provider error: %v", err)
	}
	if _, err := m.Poll(context.Background(), "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown session: %v", err)
	}
}

func TestCancelAndCapacityForgetSessions(t *testing.T) {
	m := New(map[string]Provider{"demo": fixedProvider(Status{State: Waiting})})
	first, _ := m.Start(context.Background(), "demo")
	m.Cancel(first.ID)
	if _, err := m.Poll(context.Background(), first.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("canceled session: %v", err)
	}
	for range maxSessions + 4 {
		if _, err := m.Start(context.Background(), "demo"); err != nil {
			t.Fatal(err)
		}
	}
	if len(m.sessions) != maxSessions {
		t.Fatalf("sessions = %d", len(m.sessions))
	}
}
