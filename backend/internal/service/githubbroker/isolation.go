package githubbroker

import (
	"fmt"
	"os"
	"strings"
)

// ValidateIsolation enforces that the GitHub App private key is not exposed
// inside the worker trust boundary.
//
// Under "strict" mode (the default), if PrivateKeyPath is present on disk and
// readable by the current process UID (which workers inherit when spawned
// locally without a separate OS user or container/sandbox), ValidateIsolation
// fails closed. Same-UID chmod 600 alone is insufficient because any same-UID
// worker process can read the file directly.
//
// Supported isolation modes:
//   - "strict" (default): fails closed when PrivateKeyPath is readable under
//     the same UID without an external sandbox.
//   - "external-sandbox": operator explicitly certifies that workers run inside
//     an OS/container/VM sandbox that cannot access the daemon's PrivateKeyPath.
//   - "unsafe-same-uid": explicit local-development/smoke-test override only.
func ValidateIsolation(mode, privateKeyPath string) error {
	normalized := strings.ToLower(strings.TrimSpace(mode))
	switch normalized {
	case "external-sandbox", "unsafe-same-uid":
		return nil
	case "", "strict":
		keyPath := strings.TrimSpace(privateKeyPath)
		if keyPath == "" {
			return nil
		}
		if _, err := os.Stat(keyPath); err == nil {
			// If the daemon itself can stat/read the key file under its current
			// UID and no external sandbox boundary is declared, local child
			// workers running under the same UID can also read it.
			f, openErr := os.Open(keyPath)
			if openErr == nil {
				_ = f.Close()
				return fmt.Errorf(
					"%w: private key file is readable by the daemon/worker UID (same-UID chmod 600 is insufficient); "+
						"run workers in an isolated OS/container sandbox and set AO_GITHUB_APP_WORKER_ISOLATION=external-sandbox, "+
						"or for local development/smoke testing only set AO_GITHUB_APP_WORKER_ISOLATION=unsafe-same-uid",
					ErrUnsupportedIsolation,
				)
			}
		}
		return nil
	default:
		return fmt.Errorf("%w: unknown isolation mode %q", ErrUnsupportedIsolation, mode)
	}
}
