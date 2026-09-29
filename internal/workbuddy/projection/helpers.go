package projection

// cloneMap 浅拷贝 map（不共享顶层键）。
func cloneMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// cloneMsgs 把 []any 过滤拷贝成 []map[string]any；非 map 元素丢弃
// （与 Python 版 isinstance(msg, dict) → continue 语义一致）。
func cloneMsgs(raw []any) []map[string]any {
	if raw == nil {
		return nil
	}
	out := make([]map[string]any, 0, len(raw))
	for _, m := range raw {
		if mm, ok := m.(map[string]any); ok {
			out = append(out, mm)
		}
	}
	return out
}

// toAny []map[string]any → []any（写回 body["messages"]/body["tools"] 用）。
func toAny(msgs []map[string]any) []any {
	out := make([]any, len(msgs))
	for i, m := range msgs {
		out[i] = m
	}
	return out
}
