package okx

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
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
	// JSON null / 空 body 不能被当成「空列表」：前者多为接口异常（鉴权失败、
	// 产品下架、字段改名），后者是 bug。当成空列表会让「没有持仓」和
	// 「查询失败」不可区分，进而重复下单。
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, fmt.Errorf("decode %T: 服务端 data 为 null/空", *new(T))
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

	// 形态 3：data 是单对象本身。json.Unmarshal 对任意对象都会成功（未知
	// 字段被忽略），所以必须拒绝「解析出来是零值」的情况——否则 wrapper
	// 字段改名 / data:{} 会伪造出一个 ProductID=""、OrdID="" 的假对象，
	// 让上层误以为「下单成功 / 没有持仓」。
	var single T
	if serr := json.Unmarshal(raw, &single); serr == nil {
		if v := reflect.ValueOf(single); v.Kind() != reflect.Struct || !v.IsZero() {
			return []T{single}, nil
		}
		return nil, fmt.Errorf("decode %T: data 结构不匹配（解析为零值对象）: %s", single, truncate(string(raw), 200))
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
//
// 默认按**非幂等写请求**处理：只有 50011 会重试，其余失败可能已成交，
// 返回 *AmbiguousError。查询类 POST（如 DCY 报价）请用 postDecodeIdem。
func (c *Client) postDecodeKeys[T any](path string, body any, keys ...string) (*T, error) {
	raw, err := c.doJSON(ctx(), "POST", path, body, false)
	if err != nil {
		return nil, err
	}
	return decodeKeys[T](raw, nil, keys...)
}

// postDecode POST + 单对象解包（data[0]，取不到对象返回 nil）。非幂等。
func (c *Client) postDecode[T any](path string, body any) (*T, error) {
	return c.postDecodeKeys[T](path, body)
}

// postDecodeIdem 幂等 POST（无副作用，可安全重试），用于报价类接口。
func (c *Client) postDecodeIdem[T any](path string, body any) (*T, error) {
	raw, err := c.doJSON(ctx(), "POST", path, body, true)
	if err != nil {
		return nil, err
	}
	return decodeKeys[T](raw, nil)
}
