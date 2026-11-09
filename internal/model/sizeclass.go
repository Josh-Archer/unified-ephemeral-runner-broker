package model

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
)

const (
	// DefaultSizeClass is the class used when a job does not ask for a size.
	// With no broker.sizeClasses.default entry, that class leaves the pod
	// template's resources unchanged.
	DefaultSizeClass = "default"
	// SizeLabelPrefix marks a runs-on label that selects a size class.
	// size-large selects the class large.
	SizeLabelPrefix = "size-"
)

// ErrUnknownSizeClass is returned when a job names a size the broker does not
// define, names more than one, or names a class no enabled backend can serve.
var ErrUnknownSizeClass = fmt.Errorf("unknown runner size class")

// sizeClassName matches a class key: small, large, default, build, 2xl.
var sizeClassName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$`)

// quantityPattern is a Kubernetes resource quantity: 500m, 1, 1.5, 2Gi, 16Gi.
var quantityPattern = regexp.MustCompile(`^([0-9]+(?:\.[0-9]+)?)(m|Ki|Mi|Gi|Ti|Pi|Ei|k|M|G|T|P|E)?$`)

// ResourceQuantity is one side of a container's Kubernetes resource shape.
// Empty fields are left as the pod template defined them.
type ResourceQuantity struct {
	CPU              string `yaml:"cpu,omitempty" json:"cpu,omitempty"`
	Memory           string `yaml:"memory,omitempty" json:"memory,omitempty"`
	EphemeralStorage string `yaml:"ephemeral-storage,omitempty" json:"ephemeral-storage,omitempty"`
}

// Empty reports whether every quantity is unset.
func (q ResourceQuantity) Empty() bool {
	return strings.TrimSpace(q.CPU) == "" && strings.TrimSpace(q.Memory) == "" && strings.TrimSpace(q.EphemeralStorage) == ""
}

// ContainerResources is the requests and limits overlay for one container.
type ContainerResources struct {
	Requests ResourceQuantity `yaml:"requests,omitempty" json:"requests,omitempty"`
	Limits   ResourceQuantity `yaml:"limits,omitempty" json:"limits,omitempty"`
}

// Empty reports whether the overlay sets no requests and no limits.
func (c ContainerResources) Empty() bool {
	return c.Requests.Empty() && c.Limits.Empty()
}

// SizeClassConfig is one curated runner shape. Runner applies to the actions
// runner container. Dind applies to a container named dind, in containers or
// initContainers. Backends, when non-empty, is the only set of backends that
// may run the class.
type SizeClassConfig struct {
	Runner   ContainerResources `yaml:"runner,omitempty" json:"runner,omitempty"`
	Dind     ContainerResources `yaml:"dind,omitempty" json:"dind,omitempty"`
	Backends []BackendName      `yaml:"backends,omitempty" json:"backends,omitempty"`
}

// EmptyShape reports whether the class sets no container resources.
func (c SizeClassConfig) EmptyShape() bool {
	return c.Runner.Empty() && c.Dind.Empty()
}

// SizeClassSelection is the class a job resolved to.
type SizeClassSelection struct {
	// Name is the canonical class, such as default or large.
	Name string
	// Label is the canonical size-<name> label when the job asked for a size.
	// It is empty for the implicit default.
	Label string
	// Configured is false only for the implicit default, when broker.sizeClasses
	// has no default entry.
	Configured bool
	Class      SizeClassConfig
}

// OverlaysResources reports whether pod resources should be rewritten.
func (s SizeClassSelection) OverlaysResources() bool {
	return s.Configured && !s.Class.EmptyShape()
}

// AnnotatesPod reports whether the pod should record the class. The implicit
// default leaves the pod template unchanged, including its labels.
func (s SizeClassSelection) AnnotatesPod() bool {
	return s.Configured || s.Label != ""
}

type sizeClassError struct {
	msg string
}

func (e *sizeClassError) Error() string {
	if e == nil {
		return ""
	}
	return e.msg
}

func (e *sizeClassError) Unwrap() error { return ErrUnknownSizeClass }

// SizeClassErrorf builds an error that errors.Is matches to ErrUnknownSizeClass.
func SizeClassErrorf(format string, args ...any) error {
	return &sizeClassError{msg: fmt.Sprintf(format, args...)}
}

// ValidSizeClassName reports whether name is a canonical class key.
func ValidSizeClassName(name string) bool {
	return sizeClassName.MatchString(name)
}

// ValidQuantity reports whether value is empty or a positive Kubernetes quantity.
func ValidQuantity(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return true
	}
	match := quantityPattern.FindStringSubmatch(value)
	if match == nil {
		return false
	}
	number, err := strconv.ParseFloat(match[1], 64)
	if err != nil || number <= 0 || math.IsInf(number, 0) || math.IsNaN(number) {
		return false
	}
	return true
}

// IsSizeLabel reports whether label is a size-class selector (prefix size-).
func IsSizeLabel(label string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(label)), SizeLabelPrefix)
}

// WithoutSizeLabels returns labels that are not size-class selectors.
func WithoutSizeLabels(labels []string) []string {
	if len(labels) == 0 {
		return labels
	}
	out := make([]string, 0, len(labels))
	for _, label := range labels {
		if IsSizeLabel(label) {
			continue
		}
		out = append(out, label)
	}
	return out
}

// ResolveSizeClass maps runs-on labels onto a size class.
// No size label selects default. default is valid even when it is not configured.
// Any other missing class, a malformed size label, or more than one size label fails.
func ResolveSizeClass(labels []string, classes map[string]SizeClassConfig) (SizeClassSelection, error) {
	var found []string
	for _, label := range labels {
		if IsSizeLabel(label) {
			found = append(found, strings.TrimSpace(label))
		}
	}
	if len(found) > 1 {
		quoted := make([]string, len(found))
		for i, label := range found {
			quoted[i] = fmt.Sprintf("%q", label)
		}
		return SizeClassSelection{}, SizeClassErrorf("job requests more than one runner size class (%s)", strings.Join(quoted, ", "))
	}

	name := DefaultSizeClass
	label := ""
	if len(found) == 1 {
		raw := strings.ToLower(found[0])
		name = strings.TrimPrefix(raw, SizeLabelPrefix)
		if !ValidSizeClassName(name) {
			shown := name
			if shown == "" {
				shown = found[0]
			}
			return SizeClassSelection{}, SizeClassErrorf("unknown runner size class %q", shown)
		}
		label = SizeLabelPrefix + name
	}

	class, ok := normalizeSizeClasses(classes)[name]
	if !ok {
		if name == DefaultSizeClass {
			// An explicit size-default label is still recorded so the runner
			// registers with it. No configured shape means the pod template stays.
			return SizeClassSelection{Name: name, Label: label}, nil
		}
		return SizeClassSelection{}, SizeClassErrorf("unknown runner size class %q", name)
	}
	return SizeClassSelection{Name: name, Label: label, Configured: true, Class: class}, nil
}

func normalizeSizeClasses(classes map[string]SizeClassConfig) map[string]SizeClassConfig {
	if len(classes) == 0 {
		return nil
	}
	out := make(map[string]SizeClassConfig, len(classes))
	for name, class := range classes {
		out[strings.ToLower(strings.TrimSpace(name))] = class
	}
	return out
}
