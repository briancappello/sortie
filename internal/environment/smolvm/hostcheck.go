package smolvm

import (
	"context"
	"fmt"
	"runtime"
	"time"

	"github.com/sortie-ai/sortie/internal/config"
)

// versionCheckTimeout bounds the version command run by [CheckHost].
const versionCheckTimeout = 10 * time.Second

// CheckHost checks that this host can run the smolvm kind as cfg
// configures it. It refuses a host other than Linux, the only host the
// kind is proven on, and a credential binding that
// sets a header, which smolvm's credential flag cannot express, and runs
// the configured binary's version command once, refusing a binary that cannot be run
// or whose version is older than [MinimumVersion]. Every refusal is a
// [*config.ConfigError].
func CheckHost(cfg config.EnvironmentConfig) error {
	if runtime.GOOS != "linux" {
		return &config.ConfigError{
			Field: "environment.kind",
			Message: fmt.Sprintf("the smolvm environment runs only on a Linux host; this host is %s",
				runtime.GOOS),
		}
	}
	for i, cred := range cfg.Smolvm.Credentials {
		if cred.Header != "" {
			return &config.ConfigError{
				Field: fmt.Sprintf("environment.smolvm.credentials[%d].header", i),
				Message: fmt.Sprintf("smolvm has no header option for a credential binding; remove header %q from credential %q",
					cred.Header, cred.Name),
			}
		}
	}

	cli := NewCLI(cfg.Smolvm.Binary)
	ctx, cancel := context.WithTimeout(context.Background(), versionCheckTimeout)
	defer cancel()
	version, err := cli.Version(ctx)
	if err != nil {
		return &config.ConfigError{
			Field:   "environment.smolvm.binary",
			Message: fmt.Sprintf("cannot read the version of smolvm binary %q (minimum supported: %s): %v", cli.Binary(), MinimumVersion, err),
		}
	}
	if err := CheckVersion(version); err != nil {
		return &config.ConfigError{
			Field:   "environment.smolvm.binary",
			Message: fmt.Sprintf("smolvm binary %q: %v", cli.Binary(), err),
		}
	}
	return nil
}
