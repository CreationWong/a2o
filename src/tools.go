package main

import "encoding/json"

// unwrapToolInput returns a new map with any nested "arguments" wrapper removed.
// It does not modify the input map.
func unwrapToolInput(input map[string]interface{}) map[string]interface{} {
	// start with a shallow copy
	current := make(map[string]interface{})
	for k, v := range input {
		current[k] = v
	}
	for {
		nested, exists := current["arguments"]
		if !exists {
			break
		}
		switch v := nested.(type) {
		case map[string]interface{}:
			if len(current) == 1 {
				// whole map is just arguments -> unwrap one level
				newMap := make(map[string]interface{})
				for k, val := range v {
					newMap[k] = val
				}
				current = newMap
				continue
			} else {
				// merge arguments into parent and stop
				delete(current, "arguments")
				for k, val := range v {
					current[k] = val
				}
				break
			}
		case string:
			var parsed map[string]interface{}
			if err := json.Unmarshal([]byte(v), &parsed); err == nil {
				if len(current) == 1 {
					current = parsed
					continue
				} else {
					delete(current, "arguments")
					for k, val := range parsed {
						current[k] = val
					}
					break
				}
			}
		}
		break
	}
	return current
}
