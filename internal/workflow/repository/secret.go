package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/beelinewf/wf-engine/internal/workflow/model"
	"gorm.io/gorm"
)

// SecretRepository persists secrets. GetAll is for internal template snapshots
// only and must not be exposed by HTTP handlers.
type SecretRepository interface {
	Create(ctx context.Context, key, value string) (Secret, error)
	Update(ctx context.Context, key, value string) (Secret, error)
	GetByKey(ctx context.Context, key string) (Secret, error)
	List(ctx context.Context, page, perPage int) ([]Secret, int64, error)
	Delete(ctx context.Context, key string) error
	GetAll(ctx context.Context) (map[string]string, error)
}

// Secret is metadata returned by HTTP-facing repository reads. Plaintext is
// available only through GetAll for instance snapshotting.
type Secret struct {
	Key       string
	CreatedAt time.Time
	UpdatedAt time.Time
}

type gormSecretRepository struct {
	db *gorm.DB
}

// NewSecretRepository builds the GORM-backed repository.
func NewSecretRepository(db *gorm.DB) SecretRepository {
	return &gormSecretRepository{db: db}
}

func (r *gormSecretRepository) Create(ctx context.Context, key, value string) (Secret, error) {
	now := time.Now().UTC()
	row := SecretModel{Key: key, Value: encryptSecretValue(value), CreatedAt: now, UpdatedAt: now}
	if err := r.db.WithContext(ctx).Create(&row).Error; err != nil {
		if isUniqueViolation(err) {
			return Secret{}, fmt.Errorf("%w: secret %q already exists", model.ErrConflict, key)
		}
		return Secret{}, fmt.Errorf("secret create: %w", err)
	}
	return secretFromModel(row), nil
}

func (r *gormSecretRepository) Update(ctx context.Context, key, value string) (Secret, error) {
	res := r.db.WithContext(ctx).Model(&SecretModel{}).
		Where("key = ?", key).
		Updates(map[string]any{"value": encryptSecretValue(value), "updated_at": time.Now().UTC()})
	if res.Error != nil {
		return Secret{}, fmt.Errorf("secret update: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return Secret{}, fmt.Errorf("%w: secret %s", model.ErrNotFound, key)
	}
	return r.GetByKey(ctx, key)
}

func (r *gormSecretRepository) GetByKey(ctx context.Context, key string) (Secret, error) {
	var row SecretModel
	err := r.db.WithContext(ctx).Select("key", "created_at", "updated_at").Where("key = ?", key).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Secret{}, fmt.Errorf("%w: secret %s", model.ErrNotFound, key)
	}
	if err != nil {
		return Secret{}, fmt.Errorf("secret get: %w", err)
	}
	return secretFromModel(row), nil
}

func (r *gormSecretRepository) List(ctx context.Context, page, perPage int) ([]Secret, int64, error) {
	query := r.db.WithContext(ctx).Model(&SecretModel{})
	var total int64
	if err := query.Count(&total).Error; err != nil {
		return nil, 0, fmt.Errorf("secret count: %w", err)
	}
	if page < 1 {
		page = 1
	}
	if perPage < 1 {
		perPage = 50
	}
	var rows []SecretModel
	if err := query.Select("key", "created_at", "updated_at").
		Order(`"key" ASC`).
		Offset((page - 1) * perPage).
		Limit(perPage).
		Find(&rows).Error; err != nil {
		return nil, 0, fmt.Errorf("secret list: %w", err)
	}
	items := make([]Secret, 0, len(rows))
	for _, row := range rows {
		items = append(items, secretFromModel(row))
	}
	return items, total, nil
}

func (r *gormSecretRepository) Delete(ctx context.Context, key string) error {
	res := r.db.WithContext(ctx).Delete(&SecretModel{}, "key = ?", key)
	if res.Error != nil {
		return fmt.Errorf("secret delete: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("%w: secret %s", model.ErrNotFound, key)
	}
	return nil
}

func (r *gormSecretRepository) GetAll(ctx context.Context) (map[string]string, error) {
	var rows []SecretModel
	if err := r.db.WithContext(ctx).Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("secret snapshot: %w", err)
	}
	values := make(map[string]string, len(rows))
	for _, row := range rows {
		value, err := decryptSecretValue(row.Value)
		if err != nil {
			return nil, fmt.Errorf("secret snapshot: %w", err)
		}
		values[row.Key] = value
	}
	return values, nil
}

func secretFromModel(row SecretModel) Secret {
	return Secret{Key: row.Key, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt}
}
