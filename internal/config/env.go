package config

import "os"

// Getenv reads a DELISTD_* variable. The pre-rename ERASER_* names stopped
// being read in 3.0 (docs/upgrading-to-3.0.md).
func Getenv(name string) string { return os.Getenv(name) }
