package repository

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/beelinewf/wf-engine/internal/workflow/auth"
	"github.com/beelinewf/wf-engine/internal/workflow/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ErrRoleNotFound is returned when no role row matches the lookup.
var ErrRoleNotFound = errors.New("repository: role not found")

// RoleRepository reads and writes the role catalog. The tables are the
// source of truth: configuration seeds them only while they are empty, and
// every runtime change goes through the write methods below.
type RoleRepository interface {
	// Seed inserts or refreshes every configured role and its permissions
	// in one transaction, then prunes the permission rows of those roles
	// that the configuration no longer grants. It never deletes a role: a
	// role removed from the configuration keeps its row, so an in-flight
	// token referencing it still resolves, it simply grants nothing. It
	// runs only while the tables are empty; afterwards the API owns them.
	Seed(ctx context.Context, roles []model.Role, permissions []model.RolePermission) error
	// List returns every role with its permissions, ordered by name.
	List(ctx context.Context) ([]model.Role, map[string][]string, error)
	// Get returns one role with its permissions.
	Get(ctx context.Context, name string) (model.Role, []string, error)
	// Create inserts one role with its permissions in one transaction. A
	// duplicate name is model.ErrConflict. Creating a role can never
	// remove a grant, so it never trips the holder invariant.
	Create(ctx context.Context, role model.Role, actions []string) (model.Role, error)
	// Update replaces a role's description and permission set in one
	// transaction. An unknown name is ErrRoleNotFound. A replace that
	// would leave no role granting roles:write is model.ErrConflict and
	// rolls back.
	Update(ctx context.Context, role model.Role, actions []string) (model.Role, error)
	// Delete removes a role with its permission rows and the user_roles
	// memberships naming it in one transaction, permissions and
	// memberships first because the foreign keys are ON DELETE RESTRICT.
	// An unknown name is ErrRoleNotFound. Deleting the last role granting
	// roles:write is model.ErrConflict and rolls back.
	Delete(ctx context.Context, name string) error
}

type roleRepository struct {
	db *gorm.DB
}

// NewRoleRepository builds the role repository.
func NewRoleRepository(db *gorm.DB) RoleRepository {
	return &roleRepository{db: db}
}

func (r *roleRepository) Seed(ctx context.Context, roles []model.Role, permissions []model.RolePermission) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for _, role := range roles {
			m := RoleToModel(role)
			if err := tx.Clauses(clause.OnConflict{
				Columns:   []clause.Column{{Name: "name"}},
				DoUpdates: clause.AssignmentColumns([]string{"description", "updated_at"}),
			}).Create(&m).Error; err != nil {
				return err
			}
		}
		// wanted is the exact permission set this seed claims, so the prune
		// below can tell a stale row from a current one without re-reading
		// configuration.
		wanted := make(map[string]map[string]bool, len(roles))
		for _, role := range roles {
			wanted[role.Name] = make(map[string]bool)
		}
		for _, p := range permissions {
			perm := RolePermissionToModel(p)
			if err := tx.Clauses(clause.OnConflict{
				Columns:   []clause.Column{{Name: "role"}, {Name: "action"}},
				DoUpdates: clause.AssignmentColumns([]string{"updated_at"}),
			}).Create(&perm).Error; err != nil {
				return err
			}
			if actions, ok := wanted[perm.Role]; ok {
				actions[perm.Action] = true
			}
		}
		return pruneRolePermissions(tx, wanted)
	})
}

// pruneRolePermissions deletes the permission rows of the seeded roles that
// this seed did not grant, so a re-seed cannot keep a grant the seed no
// longer carries for a role that still exists.
//
// The delete is scoped to the roles named in wanted. A role this seed does
// not mention keeps its rows entirely: the seed never deletes a role, so an
// in-flight token still resolves the name. The tables are the source of
// truth, so a leftover row grants what it says until the API changes it. A
// role named with no actions is the explicit "this role now grants nothing"
// case and has all of its rows removed.
func pruneRolePermissions(tx *gorm.DB, wanted map[string]map[string]bool) error {
	for role, actions := range wanted {
		q := tx.Where("role = ?", role)
		if len(actions) > 0 {
			placeholders := make([]string, 0, len(actions))
			args := make([]any, 0, len(actions)+1)
			for action := range actions {
				placeholders = append(placeholders, "?")
				args = append(args, action)
			}
			args = append(args, role)
			q = q.Where("action NOT IN ("+strings.Join(placeholders, ",")+") AND role = ?", args...)
		}
		if err := q.Delete(&RolePermissionModel{}).Error; err != nil {
			return err
		}
	}
	return nil
}

func (r *roleRepository) List(ctx context.Context) ([]model.Role, map[string][]string, error) {
	var rows []RoleModel
	if err := r.db.WithContext(ctx).Order("name").Find(&rows).Error; err != nil {
		return nil, nil, err
	}
	perms, err := r.permissionMap(ctx, "")
	if err != nil {
		return nil, nil, err
	}
	out := make([]model.Role, 0, len(rows))
	for _, m := range rows {
		out = append(out, RoleFromModel(m))
	}
	return out, perms, nil
}

func (r *roleRepository) Get(ctx context.Context, name string) (model.Role, []string, error) {
	var m RoleModel
	if err := r.db.WithContext(ctx).Where("name = ?", name).First(&m).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return model.Role{}, nil, ErrRoleNotFound
		}
		return model.Role{}, nil, err
	}
	perms, err := r.permissionMap(ctx, name)
	if err != nil {
		return model.Role{}, nil, err
	}
	return RoleFromModel(m), perms[name], nil
}

func (r *roleRepository) Create(ctx context.Context, role model.Role, actions []string) (model.Role, error) {
	var out model.Role
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		m := RoleToModel(role)
		if err := tx.Create(&m).Error; err != nil {
			if isUniqueViolation(err) {
				return fmt.Errorf("%w: role %q already exists", model.ErrConflict, role.Name)
			}
			return err
		}
		for _, action := range actions {
			perm := RolePermissionToModel(model.RolePermission{
				Role: role.Name, Action: action,
				CreatedAt: role.CreatedAt, UpdatedAt: role.UpdatedAt,
			})
			if err := tx.Create(&perm).Error; err != nil {
				return err
			}
		}
		out = RoleFromModel(m)
		return nil
	})
	if err != nil {
		return model.Role{}, err
	}
	return out, nil
}

func (r *roleRepository) Update(ctx context.Context, role model.Role, actions []string) (model.Role, error) {
	var out model.Role
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := lockRoleWriters(tx); err != nil {
			return err
		}
		res := tx.Model(&RoleModel{}).Where("name = ?", role.Name).
			Updates(map[string]any{"description": role.Description, "updated_at": role.UpdatedAt})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return ErrRoleNotFound
		}
		if err := tx.Where("role = ?", role.Name).Delete(&RolePermissionModel{}).Error; err != nil {
			return err
		}
		for _, action := range actions {
			perm := RolePermissionToModel(model.RolePermission{
				Role: role.Name, Action: action,
				CreatedAt: role.UpdatedAt, UpdatedAt: role.UpdatedAt,
			})
			if err := tx.Create(&perm).Error; err != nil {
				return err
			}
		}
		if err := assertRolesWriteHolder(tx); err != nil {
			return err
		}
		var m RoleModel
		if err := tx.Where("name = ?", role.Name).First(&m).Error; err != nil {
			return err
		}
		out = RoleFromModel(m)
		return nil
	})
	if err != nil {
		return model.Role{}, err
	}
	return out, nil
}

func (r *roleRepository) Delete(ctx context.Context, name string) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := lockRoleWriters(tx); err != nil {
			return err
		}
		// Permissions first: the foreign key is ON DELETE RESTRICT, so the
		// role row cannot go while a grant still names it.
		if err := tx.Where("role = ?", name).Delete(&RolePermissionModel{}).Error; err != nil {
			return err
		}
		// Memberships for the same reason: a deleted role stops granting
		// to every holder, local members included.
		if err := tx.Where("role = ?", name).Delete(&UserRoleModel{}).Error; err != nil {
			return err
		}
		res := tx.Where("name = ?", name).Delete(&RoleModel{})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return ErrRoleNotFound
		}
		return assertRolesWriteHolder(tx)
	})
}

// lockRoleWriters serializes concurrent role writes so the holder check
// below cannot pass twice. Row locks alone do not suffice: two writers
// removing distinct holders touch disjoint rows, so without a shared lock
// both would count one surviving holder and both would commit.
func lockRoleWriters(tx *gorm.DB) error {
	return tx.Exec("LOCK TABLE role_permissions IN SHARE ROW EXCLUSIVE MODE").Error
}

// assertRolesWriteHolder refuses a write that would leave no role granting
// the role-write gate, explicitly via roles:write or via the wildcard. Any
// surviving grant row is a surviving holder, so a plain count decides it,
// and running inside the write transaction rolls the mutation back.
func assertRolesWriteHolder(tx *gorm.DB) error {
	var holders int64
	if err := tx.Model(&RolePermissionModel{}).
		Where("action IN ?", []string{auth.ActionRolesWrite, auth.WildcardAction}).
		Count(&holders).Error; err != nil {
		return err
	}
	if holders == 0 {
		return fmt.Errorf("%w: at least one role must retain the roles:write permission", model.ErrConflict)
	}
	return nil
}

// permissionMap returns role name to ordered actions, restricted to one
// role when name is non-empty.
func (r *roleRepository) permissionMap(ctx context.Context, name string) (map[string][]string, error) {
	q := r.db.WithContext(ctx).Model(&RolePermissionModel{}).Order("role, action")
	if name != "" {
		q = q.Where("role = ?", name)
	}
	var rows []RolePermissionModel
	if err := q.Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make(map[string][]string, len(rows))
	for _, row := range rows {
		out[row.Role] = append(out[row.Role], row.Action)
	}
	return out, nil
}
