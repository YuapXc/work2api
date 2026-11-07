package store

import (
	"context"
	"encoding/json"

	"work2api/internal/benchmarks"
)

func (d *DB) BenchmarkBindings(ctx context.Context) (map[string]benchmarks.Binding, error) {
	rows, err := d.db.QueryContext(ctx, "SELECT model, binding FROM aa_model_bindings")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]benchmarks.Binding{}
	for rows.Next() {
		var model, raw string
		if err := rows.Scan(&model, &raw); err != nil {
			return nil, err
		}
		var binding benchmarks.Binding
		if err := json.Unmarshal([]byte(raw), &binding); err != nil {
			return nil, err
		}
		out[model] = binding
	}
	return out, rows.Err()
}

func (d *DB) SaveBenchmarkBinding(ctx context.Context, model string, binding benchmarks.Binding) error {
	if binding.EntryID == "" {
		_, err := d.db.ExecContext(ctx, "DELETE FROM aa_model_bindings WHERE model = ?", model)
		return err
	}
	raw, err := json.Marshal(binding)
	if err != nil {
		return err
	}
	_, err = d.db.ExecContext(ctx, `INSERT INTO aa_model_bindings(model, binding) VALUES (?, ?)
ON CONFLICT(model) DO UPDATE SET binding=excluded.binding`, model, string(raw))
	return err
}
