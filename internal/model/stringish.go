package model

import (
	"encoding/json"
	"fmt"
	"strconv"

	"gopkg.in/yaml.v3"
)

// Stringish unmarshals a YAML or JSON string or number as text. GitHub App
// IDs are numeric and are often written unquoted in YAML.
type Stringish string

func (s Stringish) String() string { return string(s) }

func (s *Stringish) UnmarshalYAML(value *yaml.Node) error {
	if value == nil || value.Kind == yaml.ScalarNode && (value.Tag == "!!null" || value.Value == "" && value.Tag == "") {
		*s = ""
		return nil
	}
	if value.Kind != yaml.ScalarNode {
		return fmt.Errorf("must be a string or number")
	}
	switch value.ShortTag() {
	case "!!int":
		n, err := strconv.ParseInt(value.Value, 0, 64)
		if err != nil {
			return err
		}
		*s = Stringish(strconv.FormatInt(n, 10))
		return nil
	case "!!float":
		f, err := strconv.ParseFloat(value.Value, 64)
		if err != nil {
			return err
		}
		*s = Stringish(strconv.FormatInt(int64(f), 10))
		return nil
	default:
		*s = Stringish(value.Value)
		return nil
	}
}

func (s *Stringish) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		*s = ""
		return nil
	}
	var str string
	if err := json.Unmarshal(data, &str); err == nil {
		*s = Stringish(str)
		return nil
	}
	var n int64
	if err := json.Unmarshal(data, &n); err == nil {
		*s = Stringish(strconv.FormatInt(n, 10))
		return nil
	}
	return fmt.Errorf("must be a string or number")
}
