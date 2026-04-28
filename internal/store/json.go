package store

import (
	"encoding/json"
	"fmt"
)

func marshalJSONBytes(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("marshal store json: %w", err)
	}
	return b, nil
}

func marshalJSONString(v any) (string, error) {
	b, err := marshalJSONBytes(v)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func unmarshalJSONBytes(data []byte, v any) error {
	if len(data) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("decode store json: %w", err)
	}
	return nil
}

func unmarshalJSONString(data string, v any) error {
	return unmarshalJSONBytes([]byte(data), v)
}
