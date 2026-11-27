package sessions

import (
	"os"
	"testing"

	"github.com/ArcheMind/sessionman/internal/nativedirs"
	"github.com/ArcheMind/sessionman/internal/testenv"
)

func TestMain(m *testing.M) { os.Exit(testenv.Run(m)) }

func agyRoot(home string) string { return nativedirs.AgyAppDataDir(home) }

func openCodeRoot(home string) string { return nativedirs.SessionRoots("opencode", home)[0] }
