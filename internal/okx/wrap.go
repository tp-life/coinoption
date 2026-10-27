package okx

import (
	"encoding/json"
	"fmt"
)

// wrap 把 json.RawMessage 解包到目标类型。
func wrap[T any](raw json.RawMessage, err error) ([]T, error) {
	if err != nil {
		return nil, err
	}
	var v []T
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, fmt.Errorf("decode %T: %w", v, err)
	}
	return v, nil
}

// decode 解包单个对象（取 data[0]），空数组视为 nil。
func decode[T any](raw json.RawMessage, err error) (*T, error) {
	arr, err := wrap[T](raw, err)
	if err != nil {
		return nil, err
	}
	if len(arr) == 0 {
		return nil, nil
	}
	return &arr[0], nil
}

// postDecode POST + 单对象解包（data[0]，取不到对象返回 nil）。
func (c *Client) postDecode[T any](path string, body any) (*T, error) {
	raw, err := c.doJSON(ctx(), "POST", path, body)
	if err != nil {
		return nil, err
	}
	return decode[T](raw, nil)
}
