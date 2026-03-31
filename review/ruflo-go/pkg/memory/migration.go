package memory

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"time"
)

// MigrationResult summarizes a migration run.
type MigrationResult struct {
	Total     int
	Migrated  int
	Failed    int
	Skipped   int
	Duration  time.Duration
	Errors    []string
}

// MemoryMigrator copies entries between MemoryService implementations.
type MemoryMigrator struct {
	source MemoryService
	target MemoryService
}

// NewMigrator constructs a migrator from src to dst.
func NewMigrator(src, dst MemoryService) *MemoryMigrator {
	return &MemoryMigrator{source: src, target: dst}
}

type exportEnvelope struct {
	Version int                `json:"version"`
	Entries []exportEntry      `json:"entries"`
}

type exportEntry struct {
	Key       string            `json:"key"`
	Value     string            `json:"value"`
	Namespace string            `json:"namespace"`
	Tags      []string          `json:"tags,omitempty"`
	Metadata  map[string]string `json:"metadata,omitempty"`
}

// Migrate copies all entries in one namespace from source to target.
func (m *MemoryMigrator) Migrate(ctx context.Context, namespace string) (*MigrationResult, error) {
	if m == nil || m.source == nil || m.target == nil {
		return nil, errors.New("memory: migrator requires source and target")
	}
	start := time.Now()
	res := &MigrationResult{}
	const page = 500
	offset := 0
	for {
		if ctx.Err() != nil {
			res.Duration = time.Since(start)
			return res, ctx.Err()
		}
		batch, err := m.source.List(namespace, page, offset)
		if err != nil {
			res.Duration = time.Since(start)
			return res, err
		}
		if len(batch) == 0 {
			break
		}
		for i := range batch {
			ent := &batch[i]
			res.Total++
			in := MemoryEntryInput{
				Key:       ent.Key,
				Value:     ent.Value,
				Namespace: ent.Namespace,
				Tags:      ent.Tags,
				Metadata:  ent.Metadata,
			}
			if ent.TTLSeconds != nil {
				v := *ent.TTLSeconds
				in.TTLSeconds = &v
			}
			if err := m.target.Store(in); err != nil {
				res.Failed++
				if len(res.Errors) < 32 {
					res.Errors = append(res.Errors, err.Error())
				}
				continue
			}
			res.Migrated++
		}
		offset += len(batch)
		if len(batch) < page {
			break
		}
	}
	res.Duration = time.Since(start)
	return res, nil
}

// MigrateAll copies every namespace present on the source.
func (m *MemoryMigrator) MigrateAll(ctx context.Context) (*MigrationResult, error) {
	if m == nil || m.source == nil || m.target == nil {
		return nil, errors.New("memory: migrator requires source and target")
	}
	start := time.Now()
	agg := &MigrationResult{}
	namespaces, err := m.source.ListNamespaces("")
	if err != nil {
		agg.Duration = time.Since(start)
		return agg, err
	}
	if len(namespaces) == 0 {
		r, err := m.Migrate(ctx, "")
		if err != nil {
			return r, err
		}
		return r, nil
	}
	var errs []string
	for _, ns := range namespaces {
		if ctx.Err() != nil {
			agg.Duration = time.Since(start)
			agg.Errors = append(agg.Errors, errs...)
			return agg, ctx.Err()
		}
		part, err := m.Migrate(ctx, ns)
		if part != nil {
			agg.Total += part.Total
			agg.Migrated += part.Migrated
			agg.Failed += part.Failed
			agg.Skipped += part.Skipped
			errs = append(errs, part.Errors...)
		}
		if err != nil {
			agg.Duration = time.Since(start)
			agg.Errors = append(agg.Errors, errs...)
			return agg, err
		}
	}
	agg.Duration = time.Since(start)
	if len(errs) > 0 {
		agg.Errors = errs
	}
	return agg, nil
}

// ExportJSON writes all entries from the target migrator's source to w.
func (m *MemoryMigrator) ExportJSON(ctx context.Context, w io.Writer) error {
	if m == nil || m.source == nil {
		return errors.New("memory: migrator requires source")
	}
	namespaces, err := m.source.ListNamespaces("")
	if err != nil {
		return err
	}
	env := exportEnvelope{Version: 1, Entries: nil}
	if len(namespaces) == 0 {
		if err := m.appendList(ctx, "", &env); err != nil {
			return err
		}
	} else {
		for _, ns := range namespaces {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err := m.appendList(ctx, ns, &env); err != nil {
				return err
			}
		}
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(&env)
}

func (m *MemoryMigrator) appendList(ctx context.Context, ns string, env *exportEnvelope) error {
	const page = 500
	offset := 0
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		batch, err := m.source.List(ns, page, offset)
		if err != nil {
			return err
		}
		for i := range batch {
			ent := batch[i]
			env.Entries = append(env.Entries, exportEntry{
				Key:       ent.Key,
				Value:     ent.Value,
				Namespace: ent.Namespace,
				Tags:      ent.Tags,
				Metadata:  ent.Metadata,
			})
		}
		offset += len(batch)
		if len(batch) < page {
			break
		}
	}
	return nil
}

// ImportJSON reads entries from r and stores them into the migrator's target.
func (m *MemoryMigrator) ImportJSON(ctx context.Context, r io.Reader) error {
	if m == nil || m.target == nil {
		return errors.New("memory: migrator requires target")
	}
	var env exportEnvelope
	if err := json.NewDecoder(r).Decode(&env); err != nil {
		return err
	}
	for i := range env.Entries {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		e := env.Entries[i]
		in := MemoryEntryInput{
			Key:       e.Key,
			Value:     e.Value,
			Namespace: e.Namespace,
			Tags:      e.Tags,
			Metadata:  e.Metadata,
		}
		if err := m.target.Store(in); err != nil {
			return err
		}
	}
	return nil
}
