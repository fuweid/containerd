package docker

import (
	"sync"
	"time"
)

// challengeCache stores the challenge for each request.
//
// the challenge is temporary object and should be removed after answer it.
type challengeCache struct {
	sync.Mutex

	// indexed by auth ID
	local map[string]challenge
}

func newChallengeCache() *challengeCache {
	return &challengeCache{
		local: map[string]challenge{},
	}
}

func (cc *challengeCache) set(id string, c challenge) {
	cc.Lock()
	defer cc.Unlock()

	cc.local[id] = c
}

func (cc *challengeCache) pop(id string) (challenge, bool) {
	cc.Lock()
	defer cc.Unlock()

	c, exist := cc.local[id]
	/*
		if exist {
			delete(cc.local, id)
		}
	*/
	return c, exist
}

// expiredDelta considers a token expired earlier than the actual because
// we should count the real roundtrip time.
const expiredDelta = 10 * time.Second

// tokenIndex is used to store the challenge answer as token index.
//
// NOTE: scopes is sorted.
type tokenIndex struct {
	realm    string
	service  string
	scopes   string
	username string
	secret   string
}

type token struct {
	value     string
	expiredAt time.Time
}

func (t *token) isExpired() bool {
	return t.expiredAt.Round(0).Add(-expiredDelta).Before(time.Now())
}

// tokenCache caches the auth service response
type tokenCache struct {
	sync.Mutex

	local map[tokenIndex]token
}

func newTokenCache() *tokenCache {
	return &tokenCache{
		local: map[tokenIndex]token{},
	}
}

// cleanupExpired cleans the expired token in cache.
func (tc *tokenCache) cleanupExpired() {
	for k, t := range tc.local {
		if t.isExpired() {
			delete(tc.local, k)
		}
	}
}

func (tc *tokenCache) store(idx tokenIndex, t token) {
	tc.Lock()
	defer tc.Unlock()

	tc.local[idx] = t
}

func (tc *tokenCache) lookup(idx tokenIndex) (string, bool) {
	tc.Lock()
	defer tc.Unlock()

	token, exist := tc.local[idx]
	if exist && token.isExpired() {
		tc.cleanupExpired()
	}

	token, exist = tc.local[idx]
	return token.value, exist
}
