package configops

import (
	"fmt"
	"slices"

	"github.com/pilat/coagent/internal/config"
)

// RemoveManager deletes a manager by id.
func RemoveManager(id string) Op { return &removeManager{id: id} }

type removeManager struct{ id string }

func (o *removeManager) Path() string { return "managers." + o.id }

func (o *removeManager) Summary() string { return "remove manager " + o.id }

func (o *removeManager) apply(draft *config.UnifiedConfig) error {
	before := len(draft.Managers)

	draft.Managers = slices.DeleteFunc(draft.Managers, func(m config.ManagerEntry) bool {
		return m.ID == o.id
	})

	if len(draft.Managers) == before {
		return fmt.Errorf("no manager named %q", o.id)
	}

	return nil
}
