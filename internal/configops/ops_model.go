package configops

import (
	"errors"
	"fmt"
	"slices"

	"github.com/pilat/coagent/internal/config"
)

// AddModel appends a model. Models[0] is the default, so an appended model never
// silently becomes one.
func AddModel(entry config.ModelEntry) Op { return &addModel{entry: entry} }

// RemoveModel deletes a model by id. Removing the default (Models[0]) is refused
// unless newDefault names the model that takes its place — a config with no
// default is one the daemon cannot start a session on.
func RemoveModel(id, newDefault string) Op {
	return &removeModel{id: id, newDefault: newDefault}
}

// SetDefaultModel moves a model to index 0. The list order *is* the default:
// there is no separate key to drift out of sync with it.
func SetDefaultModel(id string) Op { return &setDefaultModel{id: id} }

// SetModelTags replaces a configured model's autonomous-subagent tags.
func SetModelTags(id string, tags []string) Op { return &setModelTags{id: id, tags: tags} }

type addModel struct{ entry config.ModelEntry }

func (o *addModel) Path() string { return "models." + o.entry.ID }

func (o *addModel) Summary() string { return "add model " + o.entry.ID }

func (o *addModel) apply(draft *config.UnifiedConfig) error {
	if o.entry.ID == "" || o.entry.Provider == "" {
		return errors.New("a model needs both id and provider")
	}

	if _, ok := draft.Providers[o.entry.Provider]; !ok {
		return fmt.Errorf("no provider named %q", o.entry.Provider)
	}

	if indexOfModel(draft, o.entry.ID) >= 0 {
		return fmt.Errorf("model %q is already configured", o.entry.ID)
	}

	draft.Models = append(draft.Models, o.entry)

	return nil
}

type removeModel struct{ id, newDefault string }

func (o *removeModel) Path() string { return "models." + o.id }

func (o *removeModel) Summary() string { return "remove model " + o.id }

func (o *removeModel) apply(draft *config.UnifiedConfig) error {
	i := indexOfModel(draft, o.id)
	if i < 0 {
		return fmt.Errorf("no model named %q", o.id)
	}

	if i == 0 {
		if err := o.promoteReplacement(draft); err != nil {
			return err
		}

		i = indexOfModel(draft, o.id)
	}

	draft.Models = slices.Delete(slices.Clone(draft.Models), i, i+1)

	return nil
}

// promoteReplacement moves newDefault to the front so deleting the old default
// leaves a valid one behind.
func (o *removeModel) promoteReplacement(draft *config.UnifiedConfig) error {
	if o.newDefault == "" {
		return fmt.Errorf("%q is the default model — name its replacement to remove it", o.id)
	}

	if o.newDefault == o.id {
		return errors.New("the replacement default cannot be the model being removed")
	}

	j := indexOfModel(draft, o.newDefault)
	if j < 0 {
		return fmt.Errorf("no model named %q to make the new default", o.newDefault)
	}

	moveToFront(draft, j)

	return nil
}

type setDefaultModel struct{ id string }

type setModelTags struct {
	id   string
	tags []string
}

func (o *setDefaultModel) Path() string { return "models." + o.id }

func (o *setDefaultModel) Summary() string { return "set default model " + o.id }

func (o *setDefaultModel) apply(draft *config.UnifiedConfig) error {
	i := indexOfModel(draft, o.id)
	if i < 0 {
		return fmt.Errorf("no model named %q", o.id)
	}

	moveToFront(draft, i)

	return nil
}

func (o *setModelTags) Path() string { return "models." + o.id + ".tags" }

func (o *setModelTags) Summary() string { return "set model tags " + o.id }

func (o *setModelTags) apply(draft *config.UnifiedConfig) error {
	i := indexOfModel(draft, o.id)
	if i < 0 {
		return fmt.Errorf("no model named %q", o.id)
	}

	tags := make([]string, 0, len(o.tags))

	seen := make(map[string]struct{}, len(o.tags))

	for _, tag := range o.tags {
		if !config.ValidModelTag(tag) {
			return fmt.Errorf("invalid tag %q", tag)
		}

		if _, exists := seen[tag]; exists {
			continue
		}

		seen[tag] = struct{}{}
		tags = append(tags, tag)
	}

	draft.Models[i].Tags = tags

	return nil
}

func indexOfModel(draft *config.UnifiedConfig, id string) int {
	return slices.IndexFunc(draft.Models, func(m config.ModelEntry) bool { return m.ID == id })
}

func moveToFront(draft *config.UnifiedConfig, i int) {
	models := slices.Clone(draft.Models)
	target := models[i]

	draft.Models = append([]config.ModelEntry{target}, slices.Delete(models, i, i+1)...)
}
