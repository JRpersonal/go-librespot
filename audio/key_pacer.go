package audio

import (
	"context"
	"errors"
	"sync"
	"time"

	librespot "github.com/devgianlu/go-librespot"
)

// Spotify refuses audio keys in runs for some accounts. The refusal is an
// AesKeyError packet, not HTTP, so there is no 429, but it behaves like a rate
// limit: it starts when many keys are asked for at once (skipping through a
// playlist quickly, or walking past refused tracks one after the other), and
// asking again at once keeps it going. Waiting 3, then 5, then 10 seconds after
// a refusal before asking for another key makes it stop for those accounts
// (librespot-org/librespot#1649, the back-off of librespot-org/librespot#1844).
//
// A refusal that lasts, which is what accounts in Spotify's PlayPlay cohort get
// for every key, does not stop for any wait. So the retries are spent once per
// run of refusals: after a key has been refused through all of them, further
// keys of the same run are asked for once, still after the wait.

// ErrKeyBackingOff is returned by a single-attempt request (see
// WithSingleKeyAttempt) while the session waits after a refused key. It is not
// a refusal, so it does not wrap *KeyProviderError.
var ErrKeyBackingOff = errors.New("audio key requests are backing off after a refusal")

type keyPacerConfig struct {
	// gap is the shortest time between two key requests.
	gap time.Duration
	// backoff is how long the session waits before it asks for any key after
	// one, two, and three or more refusals in a row.
	backoff []time.Duration
	// retries is how many times a refused key is asked for again, each time
	// after the wait, and also how many refusals a run may have before its
	// refusals are treated as lasting.
	retries int
	// runQuiet ends a run of refusals: a refusal that comes this long after the
	// previous one starts a new run, with its retries.
	runQuiet time.Duration
}

var defaultKeyPacerConfig = keyPacerConfig{
	gap:      200 * time.Millisecond,
	backoff:  []time.Duration{3 * time.Second, 5 * time.Second, 10 * time.Second},
	retries:  3,
	runQuiet: time.Minute,
}

// keyPacer paces the audio key requests of a session.
type keyPacer struct {
	cfg   keyPacerConfig
	log   librespot.Logger
	now   func() time.Time
	sleep func(ctx context.Context, d time.Duration) error

	// slot is held for the whole of a request, wait included, so only one
	// request is in flight at a time.
	slot chan struct{}

	mu          sync.Mutex
	lastSent    time.Time
	refusals    int // refusals in the current run, since the last key that arrived
	lastRefusal time.Time
	until       time.Time // no key is asked for before this
}

func newKeyPacer(log librespot.Logger, cfg keyPacerConfig) *keyPacer {
	return &keyPacer{
		cfg:   cfg,
		log:   log,
		now:   time.Now,
		sleep: sleepContext,
		slot:  make(chan struct{}, 1),
	}
}

func sleepContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}

	t := time.NewTimer(d)
	defer t.Stop()

	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func isKeyRefusal(err error) bool {
	var keyErr *KeyProviderError
	return errors.As(err, &keyErr)
}

// backingOff reports whether the session is waiting after a refused key.
func (k *keyPacer) backingOff() bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.now().Before(k.until)
}

// request asks for a key with send, and asks again for a refused one after
// each wait, up to cfg.retries times, unless its run of refusals has already
// used up its retries.
func (k *keyPacer) request(ctx context.Context, send func(context.Context) ([]byte, error)) ([]byte, error) {
	for attempt := 0; ; attempt++ {
		key, run, err := k.ask(ctx, true, send)
		if !isKeyRefusal(err) {
			return key, err
		}

		if attempt >= k.cfg.retries {
			return nil, err
		} else if run > k.cfg.retries {
			// This run has outlasted every wait before: asking again only
			// holds the next track back for nothing.
			k.log.Debugf("not asking again for a refused audio key, %d refusals in a row", run)
			return nil, err
		}

		k.log.Debugf("asking again for a refused audio key (%d/%d)", attempt+1, k.cfg.retries)
	}
}

// tryOnce asks for a key once, for a caller that can do without it. While the
// session waits after a refusal it fails at once with ErrKeyBackingOff.
func (k *keyPacer) tryOnce(ctx context.Context, send func(context.Context) ([]byte, error)) ([]byte, error) {
	key, _, err := k.ask(ctx, false, send)
	return key, err
}

// ask sends one request when it is its turn: after the previous request has
// been answered, cfg.gap after it was sent, and after any wait for a refusal.
// It returns the number of refusals in the current run after its own answer.
func (k *keyPacer) ask(ctx context.Context, wait bool, send func(context.Context) ([]byte, error)) ([]byte, int, error) {
	select {
	case k.slot <- struct{}{}:
	case <-ctx.Done():
		return nil, 0, ctx.Err()
	}
	defer func() { <-k.slot }()

	k.mu.Lock()
	now := k.now()
	backoff := k.until.Sub(now)
	delay := max(backoff, k.lastSent.Add(k.cfg.gap).Sub(now))
	run := k.refusals
	k.mu.Unlock()

	if backoff > 0 && !wait {
		return nil, run, ErrKeyBackingOff
	}

	if delay > 0 {
		if err := k.sleep(ctx, delay); err != nil {
			return nil, run, err
		}
	}

	k.mu.Lock()
	k.lastSent = k.now()
	k.mu.Unlock()

	key, err := send(ctx)

	k.mu.Lock()
	defer k.mu.Unlock()

	switch {
	case err == nil:
		k.refusals = 0
		k.until = time.Time{}
	case isKeyRefusal(err):
		now := k.now()
		if k.refusals > 0 && now.Sub(k.lastRefusal) >= k.cfg.runQuiet {
			k.refusals = 0
		}
		k.refusals++
		k.lastRefusal = now

		d := k.cfg.backoff[min(k.refusals, len(k.cfg.backoff))-1]
		k.until = now.Add(d)
		k.log.Warnf("Spotify refused an audio key (%d in a row), waiting %s before asking for another: %v", k.refusals, d, err)
	default:
		// A timeout or a lost connection says nothing about whether Spotify
		// would give the key.
	}

	return key, k.refusals, err
}

type singleKeyAttemptKey struct{}

// WithSingleKeyAttempt marks ctx for a caller that can do without the audio
// key, such as a prefetch: the key is asked for once, never again after a
// refusal, and not at all while the session waits after one (ErrKeyBackingOff).
func WithSingleKeyAttempt(ctx context.Context) context.Context {
	return context.WithValue(ctx, singleKeyAttemptKey{}, true)
}

func isSingleKeyAttempt(ctx context.Context) bool {
	v, _ := ctx.Value(singleKeyAttemptKey{}).(bool)
	return v
}
