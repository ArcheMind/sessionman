package hooks

import (
	"os"
	"testing"

	"github.com/ArcheMind/agentx/internal/testenv"
)

func TestMain(m *testing.M) { os.Exit(testenv.Run(m)) }
