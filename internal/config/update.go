package config

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"go.yaml.in/yaml/v3"
)

// ConfigOp enumerates supported update operations on a configuration value.
type ConfigOp string

const (
	ConfigOpSet    ConfigOp = "set"
	ConfigOpDelete ConfigOp = "delete"
	ConfigOpAppend ConfigOp = "append"
	ConfigOpRemove ConfigOp = "remove"
)

// BackupConfig copies the configuration file to <path>.bak. It is a no-op
// when the file does not exist.
func BackupConfig(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read config for backup: %w", err)
	}
	backupPath := path + ".bak"
	if err := os.WriteFile(backupPath, data, 0o600); err != nil {
		return fmt.Errorf("write config backup: %w", err)
	}
	return nil
}

// UpdateConfigField modifies a YAML configuration file at the given
// dot-separated key path, preserving comments and other fields.
//
// Supported operations:
//   - ConfigOpSet: replace the value at keyPath with valueYAML.
//   - ConfigOpDelete: remove the key at keyPath entirely.
//   - ConfigOpAppend: parse valueYAML and append it to the sequence at keyPath.
//   - ConfigOpRemove: parse valueYAML and remove the matching element from the
//     sequence at keyPath.
func UpdateConfigField(configPath, keyPath, valueYAML string, op ConfigOp) error {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}

	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("parse config: %w", err)
	}

	root := &doc
	if doc.Kind == yaml.DocumentNode && len(doc.Content) > 0 {
		root = doc.Content[0]
	}
	if root.Kind != yaml.MappingNode {
		return errors.New("config root must be a mapping")
	}

	parts := strings.Split(keyPath, ".")

	switch op {
	case ConfigOpSet:
		if valueYAML == "" {
			return errors.New("value is required for set operation")
		}
		if err := setField(root, parts, valueYAML); err != nil {
			return err
		}
	case ConfigOpDelete:
		if err := deleteField(root, parts); err != nil {
			return err
		}
	case ConfigOpAppend:
		if valueYAML == "" {
			return errors.New("value is required for append operation")
		}
		if err := appendField(root, parts, valueYAML); err != nil {
			return err
		}
	case ConfigOpRemove:
		if valueYAML == "" {
			return errors.New("value is required for remove operation")
		}
		if err := removeField(root, parts, valueYAML); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unsupported config operation %q", op)
	}

	out, err := yaml.Marshal(&doc)
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	return os.WriteFile(configPath, out, 0o600)
}

// findMappingEntry finds a key in a mapping node and returns the index
// of the key in Content, or -1 if not found. The value is at index+1.
func findMappingEntry(mapping *yaml.Node, key string) int {
	for i := 0; i < len(mapping.Content)-1; i += 2 {
		if mapping.Content[i].Value == key {
			return i
		}
	}
	return -1
}

// parseYAMLValue parses a YAML string into a single value node (stripping
// the enclosing DocumentNode).
func parseYAMLValue(valueYAML string) (*yaml.Node, error) {
	var node yaml.Node
	if err := yaml.Unmarshal([]byte(valueYAML), &node); err != nil {
		return nil, fmt.Errorf("parse value: %w", err)
	}
	if node.Kind == yaml.DocumentNode && len(node.Content) > 0 {
		return node.Content[0], nil
	}
	return &node, nil
}

// setField navigates to the dotted path, creating intermediate mappings as
// needed, and replaces the final value with the parsed YAML value.
func setField(root *yaml.Node, parts []string, valueYAML string) error {
	if len(parts) == 0 {
		return errors.New("empty config path")
	}

	parent := root
	for i := 0; i < len(parts)-1; i++ {
		part := parts[i]
		idx := findMappingEntry(parent, part)
		if idx == -1 {
			keyNode := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: part}
			valNode := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			parent.Content = append(parent.Content, keyNode, valNode)
			parent = valNode
		} else {
			valNode := parent.Content[idx+1]
			if valNode.Kind != yaml.MappingNode {
				valNode.Kind = yaml.MappingNode
				valNode.Tag = "!!map"
				valNode.Content = nil
				valNode.Value = ""
			}
			parent = valNode
		}
	}

	actualValue, err := parseYAMLValue(valueYAML)
	if err != nil {
		return err
	}

	lastKey := parts[len(parts)-1]
	idx := findMappingEntry(parent, lastKey)
	if idx == -1 {
		keyNode := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: lastKey}
		parent.Content = append(parent.Content, keyNode, actualValue)
	} else {
		parent.Content[idx+1] = actualValue
	}
	return nil
}

// deleteField removes a key at the dotted path. It is a no-op when the key
// does not exist.
func deleteField(root *yaml.Node, parts []string) error {
	if len(parts) == 0 {
		return errors.New("empty config path")
	}

	parent := root
	for i := 0; i < len(parts)-1; i++ {
		idx := findMappingEntry(parent, parts[i])
		if idx == -1 {
			return nil // intermediate key missing; nothing to delete
		}
		valNode := parent.Content[idx+1]
		if valNode.Kind != yaml.MappingNode {
			return nil // not a mapping at the intermediate path
		}
		parent = valNode
	}

	idx := findMappingEntry(parent, parts[len(parts)-1])
	if idx == -1 {
		return nil
	}
	parent.Content = append(parent.Content[:idx], parent.Content[idx+2:]...)
	return nil
}

// appendField appends a parsed YAML value to the sequence at the dotted path.
func appendField(root *yaml.Node, parts []string, valueYAML string) error {
	seqNode, lastKey, err := navigateToSequence(root, parts)
	if err != nil {
		return err
	}
	if seqNode.Kind != yaml.SequenceNode {
		return fmt.Errorf("config key %q is not an array", lastKey)
	}

	actualValue, err := parseYAMLValue(valueYAML)
	if err != nil {
		return err
	}

	seqNode.Content = append(seqNode.Content, actualValue)
	return nil
}

// removeField removes a parsed YAML value from the sequence at the dotted path.
func removeField(root *yaml.Node, parts []string, valueYAML string) error {
	seqNode, lastKey, err := navigateToSequence(root, parts)
	if err != nil {
		return err
	}
	if seqNode.Kind != yaml.SequenceNode {
		return fmt.Errorf("config key %q is not an array", lastKey)
	}

	actualValue, err := parseYAMLValue(valueYAML)
	if err != nil {
		return err
	}

	for i, elem := range seqNode.Content {
		if nodeEqual(elem, actualValue) {
			seqNode.Content = append(seqNode.Content[:i], seqNode.Content[i+1:]...)
			return nil
		}
	}

	return fmt.Errorf("value not found in array at %q", lastKey)
}

// navigateToSequence traverses the dotted path and returns the sequence node
// at the final key, along with the final key name for error messages.
func navigateToSequence(root *yaml.Node, parts []string) (*yaml.Node, string, error) {
	if len(parts) == 0 {
		return nil, "", errors.New("empty config path")
	}

	parent := root
	for i := 0; i < len(parts)-1; i++ {
		idx := findMappingEntry(parent, parts[i])
		if idx == -1 {
			return nil, "", fmt.Errorf("path %q does not exist", strings.Join(parts[:i+1], "."))
		}
		valNode := parent.Content[idx+1]
		if valNode.Kind != yaml.MappingNode {
			return nil, "", fmt.Errorf("path %q is not a mapping", strings.Join(parts[:i+1], "."))
		}
		parent = valNode
	}

	lastKey := parts[len(parts)-1]
	idx := findMappingEntry(parent, lastKey)
	if idx == -1 {
		return nil, "", fmt.Errorf("config key %q does not exist", lastKey)
	}

	return parent.Content[idx+1], lastKey, nil
}

// nodeEqual compares two YAML nodes by their marshalled representation.
func nodeEqual(a, b *yaml.Node) bool {
	aBytes, aErr := yaml.Marshal(a)
	bBytes, bErr := yaml.Marshal(b)
	if aErr != nil || bErr != nil {
		return false
	}
	return string(aBytes) == string(bBytes)
}
