package auth

import (
	"context"
	"errors"
	"sync"
	"time"
)

type TokenSource struct {
	client *OpenAI
	store  Store
	name   string

	mu     sync.Mutex
	flight *refreshFlight
}

type refreshFlight struct {
	done  chan struct{}
	value OAuthCredential
	err   error
}

func NewTokenSource(client *OpenAI, store Store, credentialName string) *TokenSource {
	return &TokenSource{client: client, store: store, name: credentialName}
}

// Token returns a usable OAuth credential, refreshing five minutes early. A
// rotated refresh token is atomically persisted before the result is returned.
func (s *TokenSource) Token(ctx context.Context) (OAuthCredential, error) {
	value, err := s.store.Get(ctx, s.name)
	if err != nil {
		return OAuthCredential{}, err
	}
	if value.Type != CredentialOAuth || value.OAuth == nil {
		return OAuthCredential{}, errors.New("auth: credential is not OAuth")
	}
	if value.OAuth.ExpiresAt.After(s.client.now().Add(5 * time.Minute)) {
		return *value.OAuth, nil
	}

	s.mu.Lock()
	if flight := s.flight; flight != nil {
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			return OAuthCredential{}, ctx.Err()
		case <-flight.done:
			return flight.value, flight.err
		}
	}
	// A previous flight may have completed after this caller's initial read.
	// Re-read under the flight lock before electing another refresher.
	latest, latestErr := s.store.Get(ctx, s.name)
	if latestErr == nil && latest.Type == CredentialOAuth && latest.OAuth != nil && latest.OAuth.ExpiresAt.After(s.client.now().Add(5*time.Minute)) {
		s.mu.Unlock()
		return *latest.OAuth, nil
	}
	if latestErr != nil {
		s.mu.Unlock()
		return OAuthCredential{}, latestErr
	}
	if latest.Type != CredentialOAuth || latest.OAuth == nil {
		s.mu.Unlock()
		return OAuthCredential{}, errors.New("auth: credential is not OAuth")
	}
	value = latest
	flight := &refreshFlight{done: make(chan struct{})}
	s.flight = flight
	s.mu.Unlock()

	flight.value, flight.err = s.client.Refresh(ctx, *value.OAuth)
	if flight.err == nil {
		flight.err = s.store.Put(ctx, s.name, NewOAuthCredential(flight.value))
	}
	s.mu.Lock()
	s.flight = nil
	close(flight.done)
	s.mu.Unlock()
	return flight.value, flight.err
}
