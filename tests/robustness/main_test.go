//go:build linux

package robustness

import (
	"flag"
	"os"
	"testing"

	testutils "github.com/MPCherry/bbolt/tests/utils"
)

func TestMain(m *testing.M) {
	flag.Parse()
	testutils.RequiresRoot()
	os.Exit(m.Run())
}
