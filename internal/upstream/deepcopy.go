package upstream

// deepCopyJSON 深拷贝 encoding/json 解码出的通用值（map[string]any / []any / 标量）。
// 也兼容入站转换层直接构造的 []map[string]any（Responses 路径）。其他类型按值共享
// （字符串/数字/布尔不可变；出站改写不会原地修改未知类型）。
func deepCopyJSON(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, vv := range t {
			out[k] = deepCopyJSON(vv)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, vv := range t {
			out[i] = deepCopyJSON(vv)
		}
		return out
	case []map[string]any:
		out := make([]map[string]any, len(t))
		for i, vv := range t {
			out[i] = deepCopyJSON(vv).(map[string]any)
		}
		return out
	default:
		return v
	}
}
