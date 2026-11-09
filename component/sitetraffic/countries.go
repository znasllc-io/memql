package sitetraffic

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/oschwald/geoip2-golang"
	"github.com/uptrace/bun"

	"github.com/znasllc-io/memql/core/common"
)

// CountriesComponentName is the bus-side name of the country database loader.
const CountriesComponentName = common.ComponentName("sitetraffic.countries")

const countryRecheckEvery = time.Hour

// CountryLoader holds the current country database (an MMDB kept in
// edge_geo_country) as an in-memory reader.
//
// Country is an atomic load plus an in-memory lookup, so it is safe on the
// request path. The database is read only at Start and on an hourly check of
// the stored release label (cheap: the blob is fetched only when the label
// changed), so a monthly refresh reaches every replica without a restart; a
// check that fails is tried again sooner, from thirty seconds doubling back
// up to the hour (retry.go). No row or an unreadable one means no country,
// never a failed request.
type CountryLoader struct {
	logger   *slog.Logger
	provider func() *bun.DB

	reader  atomic.Pointer[geoip2.Reader]
	release atomic.Value // string; the release the reader was built from

	// newTimer starts the wait for the next check, answering the channel
	// that fires and a stop. A field so a test can watch the waits.
	newTimer func(d time.Duration) (<-chan time.Time, func() bool)

	running atomic.Bool
	readyCh chan struct{}
	stop    chan struct{}
	once    sync.Once
}

var _ common.Dependency = (*CountryLoader)(nil)

func NewCountryLoader(logger *slog.Logger, provider func() *bun.DB) *CountryLoader {
	if logger == nil {
		logger = slog.Default()
	}
	return &CountryLoader{
		logger:   logger.With("component", "sitetraffic.countries"),
		provider: provider,
		readyCh:  make(chan struct{}),
		stop:     make(chan struct{}),
		newTimer: systemTimer,
	}
}

// Country returns the ISO 3166 alpha-2 code for addr, or "" when no database
// is loaded or the address is not in it.
func (c *CountryLoader) Country(addr netip.Addr) string {
	r := c.reader.Load()
	if r == nil || !addr.IsValid() {
		return ""
	}
	rec, err := r.Country(addr.AsSlice())
	if err != nil || rec == nil {
		return ""
	}
	return rec.Country.IsoCode
}

// refresh loads the stored database when its release differs from the held
// one, and unloads it when the row is gone.
func (c *CountryLoader) refresh(ctx context.Context) error {
	db := c.provider()
	if db == nil {
		return fmt.Errorf("no database handle")
	}
	var release string
	err := db.NewRaw("SELECT release FROM edge_geo_country WHERE id = 1").Scan(ctx, &release)
	if errors.Is(err, sql.ErrNoRows) {
		c.swap(nil, "")
		return nil
	}
	if err != nil {
		return fmt.Errorf("read release: %w", err)
	}
	if held, _ := c.release.Load().(string); held == release && c.reader.Load() != nil {
		return nil
	}
	var data []byte
	if err := db.NewRaw("SELECT data FROM edge_geo_country WHERE id = 1").Scan(ctx, &data); err != nil {
		return fmt.Errorf("read database: %w", err)
	}
	r, err := geoip2.FromBytes(data)
	if err != nil {
		return fmt.Errorf("open release %s: %w", release, err)
	}
	c.swap(r, release)
	c.logger.Info("country database loaded", "release", release)
	return nil
}

func (c *CountryLoader) swap(r *geoip2.Reader, release string) {
	c.reader.Store(r)
	c.release.Store(release)
	// A lookup in flight may still hold the old reader; its bytes are a Go slice
	// (FromBytes), so there is no mapping to unmap and the GC frees it.
}

func (c *CountryLoader) Start(ctx context.Context) {
	defer close(c.readyCh)
	for i := 0; i < 10 && c.provider() == nil; i++ {
		time.Sleep(100 * time.Millisecond)
	}
	failures := 0
	if c.check(ctx) != nil {
		failures = 1
	}
	c.running.Store(true)
	go func() {
		for {
			fired, stop := c.newTimer(retryWait(failures, countryRecheckEvery))
			select {
			case <-fired:
				if c.check(ctx) != nil {
					failures++
				} else {
					failures = 0
				}
			case <-c.stop:
				stop()
				return
			case <-ctx.Done():
				stop()
				return
			}
		}
	}()
}

// check refreshes the held database and answers the refresh's failure, which
// Start's loop retries sooner.
func (c *CountryLoader) check(ctx context.Context) error {
	rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	err := c.refresh(rctx)
	if err != nil {
		c.logger.Warn("could not load the country database; visits are recorded without a country until it loads", "err", err)
	}
	return err
}

func (c *CountryLoader) Stop(ctx context.Context) {
	c.once.Do(func() { close(c.stop) })
	c.running.Store(false)
}

func (c *CountryLoader) IsRunning() bool { return c.running.Load() }
func (c *CountryLoader) Order() int      { return 51 }
func (c *CountryLoader) ComponentName() common.ComponentName {
	return CountriesComponentName
}
func (c *CountryLoader) Ready() <-chan struct{} { return c.readyCh }
