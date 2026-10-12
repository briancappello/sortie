package smolvm

import (
	"context"
	"fmt"
	"time"

	"github.com/sortie-ai/sortie/internal/config"
)

// versionCheckTimeout bounds the version command run by [CheckHost].
const versionCheckTimeout = 10 * time.Second

// CheckHost checks that this host can run the smolvm kind as cfg
// configures it. It refuses a credential binding that sets a header,
// which smolvm's credential flag cannot express, and runs the configured
// binary's version command once, refusing a binary that cannot be run
// or whose version is outside [SupportedVersion]. Every refusal is a
// [*config.ConfigError].
func CheckHost(cfg config.EnvironmentConfig) error {
	for i, cred := range cfg.Smolvm.Credentials {
		if cred.Header != "" {
			return &config.ConfigError{
				Field: fmt.Sprintf("environment.smolvm.credentials[%d].header", i),
				Message: fmt.Sprintf("smolvm %s has no header option for a credential binding; remove header %q from credential %q",
					SupportedVersion, cred.Header, cred.Name),
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
			Message: fmt.Sprintf("cannot read the version of smolvm binary %q (supported: %s): %v", cli.Binary(), SupportedVersion, err),
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
