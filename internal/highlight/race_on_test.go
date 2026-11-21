//go:build race

package highlight

// The race detector slows regex-heavy code roughly twenty-fold.
const slowdown = 25
