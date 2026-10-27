package okx

import (
	"encoding/json"
	"fmt"
)

// wrap 把 json.RawMessage 解包到目标类型。
func wrap[T any](raw json.RawMessage, err error) ([]T, error) {
	return wrapKeys[T](raw, err)
}

// wrapKeys 兼容三种响应形态（M0/M1 实测：
//  1. 标准：data = [...]                       （期权/现货）
//  2. 金融产品：data = {"products":[...]}      （sfp/dcd 系列）
//  3. 单对象：data = {...} → 包成单元素数组
//
// keys 为形态 2 时按序尝试的候选字段名。
func wrapKeys[T any](raw json.RawMessage, err error, keys ...string) ([]T, error) {
	if err != nil {
		return nil, err
	}

	// 形态 1：data 即数组
	var arr []T
	if aerr := json.Unmarshal(raw, &arr); aerr == nil {
		return arr, nil
	}

	// 形态 2：data 是对象，数组在 keys 指定的字段下
	var obj map[string]json.RawMessage
	if oerr := json.Unmarshal(raw, &obj); oerr == nil {
		for _, k := range keys {
			if v, has := obj[k]; has {
				if aerr := json.Unmarshal(v, &arr); aerr == nil {
					return arr, nil
				}
			}
		}
	}

	// 形态 3：data 是单对象本身
	var single T
	if serr := json.Unmarshal(raw, &single); serr == nil {
		return []T{single}, nil
	}

	return nil, fmt.Errorf("decode %T: 无法识别的 data 形态: %s", arr, truncate(string(raw), 200))
}

// decodeKeys 解包单个对象（data[0]；空数组返回 nil；形态2 时先按 keys 取数组）。
func decodeKeys[T any](raw json.RawMessage, err error, keys ...string) (*T, error) {
	arr, err := wrapKeys[T](raw, err, keys...)
	if err != nil {
		return nil, err
	}
	if len(arr) == 0 {
		return nil, nil
	}
	return &arr[0], nil
}

// decode 解包单个对象（取 data[0]），空数组视为 nil。
func decode[T any](raw json.RawMessage, err error) (*T, error) {
	return decodeKeys[T](raw, err)
}

// postDecodeKeys POST + keys 解包（取 data[0]；空数组返回 nil）。
func (c *Client) postDecodeKeys[T any](path string, body any, keys ...string) (*T, error) {
	raw, err := c.doJSON(ctx(), "POST", path, body)
	if err != nil {
		return nil, err
	}
	return decodeKeys[T](raw, nil, keys...)
}

// postDecode POST + 单对象解包（data[0]，取不到对象返回 nil）。
func (c *Client) postDecode[T any](path string, body any) (*T, error) {
	return c.postDecodeKeys[T](path, body)
}
