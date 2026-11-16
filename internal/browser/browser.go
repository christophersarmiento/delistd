package browser

import (
	"context"
	"fmt"
	"os"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/remote"

	"github.com/drumandbytes/delistd/internal/config"
)

// Browser is one Chrome session, started on first use and reused for every
// page after, as `delistd fill --pending` fills several forms in a row.
type Browser struct {
	allocCtx       context.Context
	allocCancel    context.CancelFunc
	ctx            context.Context
	cancel         context.CancelFunc
	config         BrowserConfig
	profile        *config.Profile
	allowedDomains []string
}

// extraAllocatorOptions is added to every launch; tests use it to run Chrome
// without its sandbox on CI runners that cannot provide one.
var extraAllocatorOptions []chromedp.ExecAllocatorOption

// New prepares a Browser; Chrome itself starts on the first page. A non-empty
// allowedDomains limits which hosts NavigateAndFill will visit and fill with
// personal data (see matchesAllowedDomain); empty means no check.
func New(cfg BrowserConfig, profile *config.Profile, allowedDomains []string) (*Browser, error) {
	// an explicit Chrome path means launch that one, whatever the environment says
	if cfg.RemoteURL == "" && cfg.ExecutablePath == "" {
		cfg.RemoteURL = os.Getenv(config.ChromeURLEnv)
	}

	var allocCtx context.Context
	var allocCancel context.CancelFunc
	if cfg.RemoteURL != "" {
		// Chrome runs elsewhere (another container), so the launch flags do
		// not apply; start sets the user agent and window over CDP instead
		allocCtx, allocCancel = remote.NewAllocator(context.Background(), cfg.RemoteURL)
	} else {
		opts := []chromedp.ExecAllocatorOption{
			chromedp.NoFirstRun,
			chromedp.NoDefaultBrowserCheck,
			chromedp.DisableGPU,
			chromedp.UserAgent(cfg.UserAgent),
			chromedp.WindowSize(cfg.WindowWidth, cfg.WindowHeight),
		}
		if cfg.Headless {
			opts = append(opts, chromedp.Headless)
		}
		opts = append(opts, extraAllocatorOptions...) // after the defaults, so tests can override them
		if cfg.ExecutablePath != "" {
			opts = append(opts, chromedp.ExecPath(cfg.ExecutablePath))
		}
		allocCtx, allocCancel = chromedp.NewExecAllocator(context.Background(), opts...)
	}
	ctx, cancel := chromedp.NewContext(allocCtx)
	return &Browser{allocCtx: allocCtx, allocCancel: allocCancel, ctx: ctx, cancel: cancel,
		config: cfg, profile: profile, allowedDomains: allowedDomains}, nil
}

// start connects to or launches Chrome. The first Run's context owns the
// browser, so it has to be b.ctx and not a per-call timeout context, which
// would close Chrome when the call returned. Calling it again is harmless:
// later runs reuse the connection and re-apply the same emulation.
func (b *Browser) start() error {
	var setup []chromedp.Action[chromedp.Void]
	if b.config.RemoteURL != "" {
		setup = append(setup,
			chromedp.Func(func(ctx context.Context, t *chromedp.Target) error {
				_, err := cdp.Call(ctx, t, emulation.SetUserAgentOverride, emulation.SetUserAgentOverrideParams{UserAgent: b.config.UserAgent})
				return err
			}),
			chromedp.EmulateViewport(int64(b.config.WindowWidth), int64(b.config.WindowHeight)))
	}
	if err := chromedp.Do(b.ctx, setup...); err != nil {
		if b.config.RemoteURL == "" && config.InContainer() {
			return fmt.Errorf("%w (this image has no browser: set %s to a Chrome DevTools endpoint, see docs/docker.md)", err, config.ChromeURLEnv)
		}
		return err
	}
	return nil
}

// Close shuts Chrome down.
func (b *Browser) Close() {
	if b.cancel != nil {
		b.cancel()
	}
	if b.allocCancel != nil {
		b.allocCancel()
	}
}
