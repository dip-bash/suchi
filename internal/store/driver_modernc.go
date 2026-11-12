package store

// Default driver: modernc.org/sqlite is pure Go, so suchi cross-compiles
// without a C toolchain (Termux, ARM, static binaries).
import _ "modernc.org/sqlite"

const driverName = "sqlite"
