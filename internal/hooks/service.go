package hooks

import (
	"os"

	"github.com/ArcheMind/sessionman/internal/hookmount"
)

// Service reads unified hooks from the User Config dir only; without it
// (ConfigDir empty) no hook is defined.
type Service struct {
	Home      string
	ConfigDir string
}

func New() Service {
	home, _ := os.UserHomeDir()
	return Service{Home: home, ConfigDir: os.Getenv("SM_CONFIG_DIR")}
}

func (s Service) List() (hookmount.ListResult, error) { return hookmount.Load(s.ConfigDir) }
