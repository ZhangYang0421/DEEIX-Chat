package conversation

import (
	"context"
	"errors"
	"strings"
	"time"

	domainconversation "github.com/DEEIX-AI/DEEIX-Chat/backend/internal/domain/conversation"
	domainuser "github.com/DEEIX-AI/DEEIX-Chat/backend/internal/domain/user"
	models "github.com/DEEIX-AI/DEEIX-Chat/backend/internal/infra/persistence/models"
	"github.com/DEEIX-AI/DEEIX-Chat/backend/internal/repository"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// 文件对象、文件列表与用户存储配额仓储实现。

// ListFileObjectsByUser 分页查询用户文件。
func (r *Repo) ListFileObjectsByUser(ctx context.Context, userID uint, offset int, limit int) ([]domainconversation.FileObject, int64, error) {
	return r.ListFileObjectsByUserWithFilter(ctx, userID, offset, limit, "", "all", "created")
}

func (r *Repo) ListFileObjectsByUserWithFilter(
	ctx context.Context,
	userID uint,
	offset int,
	limit int,
	searchQuery string,
	filterKind string,
	sortBy string,
) ([]domainconversation.FileObject, int64, error) {
	items := make([]models.FileObject, 0)
	var total int64

	query := r.db.WithContext(ctx).
		Model(&models.FileObject{}).
		Where("user_id = ? AND status = ?", userID, "active")
	normalizedQuery := strings.TrimSpace(searchQuery)
	if normalizedQuery != "" {
		pattern := "%" + strings.ToLower(normalizedQuery) + "%"
		query = query.Where(
			"LOWER(file_id) LIKE ? OR LOWER(file_name) LIKE ? OR LOWER(mime_type) LIKE ? OR LOWER(purpose) LIKE ? OR LOWER(sha256) LIKE ?",
			pattern,
			pattern,
			pattern,
			pattern,
			pattern,
		)
	}
	if condition, args := buildFileKindWhereClause(filterKind); condition != "" {
		query = query.Where(condition, args...)
	}
	if err := query.Count(&total).Error; err != nil {
		return nil, 0, translateError(err)
	}
	orderQuery := query
	switch sortBy {
	case "name":
		orderQuery = orderQuery.Order("file_name ASC").Order("id DESC")
	case "size":
		orderQuery = orderQuery.Order("size_bytes DESC").Order("id DESC")
	case "last_used":
		orderQuery = orderQuery.Order("COALESCE(last_accessed_at, created_at) DESC").Order("id DESC")
	default:
		orderQuery = orderQuery.Order("created_at DESC").Order("id DESC")
	}
	if err := orderQuery.
		Offset(offset).
		Limit(limit).
		Find(&items).Error; err != nil {
		return nil, 0, translateError(err)
	}
	return toFileObjectDomains(items), total, nil
}

// GetActiveFileObjectsByIDs 查询用户激活文件对象。
func (r *Repo) GetActiveFileObjectsByIDs(ctx context.Context, userID uint, fileIDs []string) ([]domainconversation.FileObject, error) {
	items := make([]models.FileObject, 0)
	if len(fileIDs) == 0 {
		return []domainconversation.FileObject{}, nil
	}
	if err := r.db.WithContext(ctx).
		Where("user_id = ? AND status = ? AND file_id IN ?", userID, "active", fileIDs).
		Find(&items).Error; err != nil {
		return nil, translateError(err)
	}
	return toFileObjectDomains(items), nil
}

// GetActiveFileObjectByID 查询单个用户激活文件对象。
func (r *Repo) GetActiveFileObjectByID(ctx context.Context, userID uint, fileID string) (*domainconversation.FileObject, error) {
	var item models.FileObject
	if err := r.db.WithContext(ctx).
		Where("user_id = ? AND status = ? AND file_id = ?", userID, "active", fileID).
		First(&item).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrFileNotFound
		}
		return nil, translateError(err)
	}
	result := toFileObjectDomain(item)
	return &result, nil
}

// RenameFileObjectByID 更新文件名。
func (r *Repo) RenameFileObjectByID(ctx context.Context, userID uint, fileID string, fileName string) (*domainconversation.FileObject, error) {
	var item models.FileObject
	if err := r.db.WithContext(ctx).
		Where("user_id = ? AND status = ? AND file_id = ?", userID, "active", fileID).
		First(&item).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrFileNotFound
		}
		return nil, translateError(err)
	}

	item.FileName = fileName
	if err := r.db.WithContext(ctx).Save(&item).Error; err != nil {
		return nil, translateError(err)
	}
	result := toFileObjectDomain(item)
	return &result, nil
}

// UpdateFileObjectRagOptOut 更新文件 RAG 检索开关。
func (r *Repo) UpdateFileObjectRagOptOut(ctx context.Context, userID uint, fileID string, ragOptOut bool) (*domainconversation.FileObject, error) {
	var item models.FileObject
	if err := r.db.WithContext(ctx).
		Where("user_id = ? AND status = ? AND file_id = ?", userID, "active", fileID).
		First(&item).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrFileNotFound
		}
		return nil, translateError(err)
	}
	item.RagOptOut = ragOptOut
	if err := r.db.WithContext(ctx).Save(&item).Error; err != nil {
		return nil, translateError(err)
	}
	result := toFileObjectDomain(item)
	return &result, nil
}

// TouchFileObjectLastAccessedAt 更新文件最近使用时间。
func (r *Repo) TouchFileObjectLastAccessedAt(ctx context.Context, userID uint, fileID string, accessedAt time.Time) error {
	return translateError(r.db.WithContext(ctx).
		Model(&models.FileObject{}).
		Where("user_id = ? AND status = ? AND file_id = ?", userID, "active", fileID).
		Update("last_accessed_at", accessedAt).Error)
}

// GetLatestActiveFileObjectBySHA 查询用户最近上传的同内容文件（按 SHA256 + Size）。
func (r *Repo) GetLatestActiveFileObjectBySHA(
	ctx context.Context,
	userID uint,
	sha256Value string,
	sizeBytes int64,
) (*domainconversation.FileObject, error) {
	var item models.FileObject
	if err := r.db.WithContext(ctx).
		Where("user_id = ? AND status = ? AND sha256 = ? AND size_bytes = ?", userID, "active", sha256Value, sizeBytes).
		Order("id DESC").
		Limit(1).
		First(&item).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, translateError(err)
	}
	result := toFileObjectDomain(item)
	return &result, nil
}

func buildFileKindWhereClause(filterKind string) (string, []interface{}) {
	normalized := strings.ToLower(strings.TrimSpace(filterKind))
	if normalized == "" || normalized == "all" {
		return "", nil
	}

	parts := strings.Split(normalized, ",")
	conditions := make([]string, 0, len(parts))
	args := make([]interface{}, 0, len(parts)*8)
	seen := make(map[string]struct{}, len(parts))

	for _, part := range parts {
		current := strings.TrimSpace(part)
		if current == "" || current == "all" {
			continue
		}
		if _, exists := seen[current]; exists {
			continue
		}
		seen[current] = struct{}{}

		condition, conditionArgs := buildSingleFileKindWhereClause(current)
		if condition == "" {
			continue
		}
		conditions = append(conditions, condition)
		args = append(args, conditionArgs...)
	}

	if len(conditions) == 0 {
		return "", nil
	}

	return "(" + strings.Join(conditions, " OR ") + ")", args
}

func buildSingleFileKindWhereClause(filterKind string) (string, []interface{}) {
	switch filterKind {
	case "image":
		return "LOWER(mime_type) LIKE ?", []interface{}{"image/%"}
	case "audio":
		return "LOWER(mime_type) LIKE ?", []interface{}{"audio/%"}
	case "video":
		return "LOWER(mime_type) LIKE ?", []interface{}{"video/%"}
	case "pdf":
		return "(LOWER(mime_type) = ? OR LOWER(file_name) LIKE ?)", []interface{}{"application/pdf", "%.pdf"}
	case "spreadsheet":
		return "(" + strings.Join([]string{
				"LOWER(mime_type) LIKE ?",
				"LOWER(mime_type) LIKE ?",
				"LOWER(mime_type) LIKE ?",
				"LOWER(file_name) LIKE ?",
				"LOWER(file_name) LIKE ?",
				"LOWER(file_name) LIKE ?",
				"LOWER(file_name) LIKE ?",
			}, " OR ") + ")", []interface{}{
				"%spreadsheet%",
				"%excel%",
				"%csv%",
				"%.xls",
				"%.xlsx",
				"%.csv",
				"%.ods",
			}
	case "presentation":
		return "(" + strings.Join([]string{
				"LOWER(mime_type) LIKE ?",
				"LOWER(mime_type) LIKE ?",
				"LOWER(file_name) LIKE ?",
				"LOWER(file_name) LIKE ?",
				"LOWER(file_name) LIKE ?",
			}, " OR ") + ")", []interface{}{
				"%presentation%",
				"%powerpoint%",
				"%.ppt",
				"%.pptx",
				"%.odp",
			}
	case "document":
		return "(" + strings.Join([]string{
				"LOWER(mime_type) LIKE ?",
				"LOWER(mime_type) LIKE ?",
				"LOWER(mime_type) LIKE ?",
				"LOWER(file_name) LIKE ?",
				"LOWER(file_name) LIKE ?",
				"LOWER(file_name) LIKE ?",
				"LOWER(file_name) LIKE ?",
				"LOWER(file_name) LIKE ?",
			}, " OR ") + ")", []interface{}{
				"%word%",
				"%rtf%",
				"%opendocument.text%",
				"%.doc",
				"%.docx",
				"%.rtf",
				"%.odt",
				"%.pages",
			}
	case "code":
		return "(" + strings.Join([]string{
				"LOWER(mime_type) LIKE ?",
				"LOWER(mime_type) LIKE ?",
				"LOWER(mime_type) LIKE ?",
				"LOWER(mime_type) LIKE ?",
				"LOWER(mime_type) LIKE ?",
				"LOWER(mime_type) LIKE ?",
				"LOWER(mime_type) LIKE ?",
				"LOWER(mime_type) LIKE ?",
				"LOWER(mime_type) LIKE ?",
				"LOWER(mime_type) LIKE ?",
				"LOWER(mime_type) LIKE ?",
				"LOWER(file_name) LIKE ?",
				"LOWER(file_name) LIKE ?",
				"LOWER(file_name) LIKE ?",
				"LOWER(file_name) LIKE ?",
				"LOWER(file_name) LIKE ?",
				"LOWER(file_name) LIKE ?",
				"LOWER(file_name) LIKE ?",
				"LOWER(file_name) LIKE ?",
				"LOWER(file_name) LIKE ?",
				"LOWER(file_name) LIKE ?",
				"LOWER(file_name) LIKE ?",
				"LOWER(file_name) LIKE ?",
				"LOWER(file_name) LIKE ?",
				"LOWER(file_name) LIKE ?",
				"LOWER(file_name) LIKE ?",
			}, " OR ") + ")", []interface{}{
				"text/%",
				"%json%",
				"%javascript%",
				"%typescript%",
				"%xml%",
				"%html%",
				"%css%",
				"%yaml%",
				"%toml%",
				"%sql%",
				"%markdown%",
				"%.js",
				"%.jsx",
				"%.ts",
				"%.tsx",
				"%.json",
				"%.html",
				"%.css",
				"%.md",
				"%.xml",
				"%.yaml",
				"%.yml",
				"%.toml",
				"%.sql",
				"%.sh",
				"%.py",
			}
	default:
		return "", nil
	}
}

// CreateFileObjectAndConsumeQuota 创建文件对象并扣减配额。
func (r *Repo) CreateFileObjectAndConsumeQuota(
	ctx context.Context,
	item *domainconversation.FileObject,
	defaultQuotaBytes int64,
) (*domainconversation.StorageQuota, error) {
	var updatedQuota models.UserStorageQuota

	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		entity := toFileObjectModel(item)
		quota, err := getOrInitQuotaForUpdate(tx, entity.UserID, defaultQuotaBytes)
		if err != nil {
			return translateError(err)
		}

		nextUsed := quota.UsedBytes + entity.SizeBytes
		if quota.QuotaBytes > 0 && nextUsed+quota.ReservedBytes > quota.QuotaBytes {
			return ErrStorageQuotaExceeded
		}

		if err = tx.Create(&entity).Error; err != nil {
			return translateError(err)
		}
		*item = toFileObjectDomain(entity)

		if err = tx.Model(&models.UserStorageQuota{}).
			Where("id = ?", quota.ID).
			Update("used_bytes", nextUsed).Error; err != nil {
			return translateError(err)
		}

		if err = tx.Where("id = ?", quota.ID).First(&updatedQuota).Error; err != nil {
			return translateError(err)
		}
		return nil
	})
	if err != nil {
		return nil, translateError(err)
	}

	result := toStorageQuotaDomain(updatedQuota)
	return &result, nil
}

// DeleteFileObjectAndReleaseQuota 删除文件对象并释放配额，可按需要求文件未被活跃会话引用。
func (r *Repo) DeleteFileObjectAndReleaseQuota(
	ctx context.Context,
	userID uint,
	fileID string,
	defaultQuotaBytes int64,
	options repository.DeleteFileObjectOptions,
) (*domainconversation.FileObject, *domainconversation.StorageQuota, bool, error) {
	var deletedFile models.FileObject
	var updatedQuota models.UserStorageQuota
	shouldRemovePhysical := false

	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("user_id = ? AND file_id = ? AND status = ?", userID, fileID, "active").
			First(&deletedFile).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrFileNotFound
			}
			return translateError(err)
		}

		if options.RequireUnreferenced {
			if err := ensureFileObjectUnreferencedByActiveConversations(tx, userID, fileID); err != nil {
				return err
			}
		}
		if err := ensureFileObjectUnreferencedByUserAvatars(tx, fileID); err != nil {
			return err
		}
		if err := ensureFileObjectUnreferencedByKnowledgeBases(tx, deletedFile.ID); err != nil {
			return err
		}

		quota, err := getOrInitQuotaForUpdate(tx, userID, defaultQuotaBytes)
		if err != nil {
			return translateError(err)
		}

		if err = tx.Model(&models.FileObject{}).
			Where("id = ?", deletedFile.ID).
			Updates(map[string]interface{}{
				"status": "deleted",
			}).Error; err != nil {
			return translateError(err)
		}

		var remainingUserRefs int64
		if err = tx.Model(&models.FileObject{}).
			Where("user_id = ? AND status = ? AND storage_path = ? AND id <> ?",
				userID,
				"active",
				deletedFile.StoragePath,
				deletedFile.ID,
			).
			Count(&remainingUserRefs).Error; err != nil {
			return translateError(err)
		}

		nextUsed := quota.UsedBytes
		if remainingUserRefs == 0 {
			nextUsed = quota.UsedBytes - deletedFile.SizeBytes
			if nextUsed < 0 {
				nextUsed = 0
			}
			if err = tx.Model(&models.UserStorageQuota{}).
				Where("id = ?", quota.ID).
				Update("used_bytes", nextUsed).Error; err != nil {
				return translateError(err)
			}
		}

		var remainingPhysicalRefs int64
		if err = tx.Model(&models.FileObject{}).
			Where("status = ? AND storage_path = ? AND id <> ?", "active", deletedFile.StoragePath, deletedFile.ID).
			Count(&remainingPhysicalRefs).Error; err != nil {
			return translateError(err)
		}
		shouldRemovePhysical = remainingPhysicalRefs == 0

		if err = tx.Where("id = ?", quota.ID).First(&updatedQuota).Error; err != nil {
			return translateError(err)
		}
		return nil
	})
	if err != nil {
		return nil, nil, false, translateError(err)
	}

	deleted := toFileObjectDomain(deletedFile)
	quota := toStorageQuotaDomain(updatedQuota)
	return &deleted, &quota, shouldRemovePhysical, nil
}

// GetOrInitUserStorageQuota 查询或初始化用户存储配额。
func (r *Repo) GetOrInitUserStorageQuota(
	ctx context.Context,
	userID uint,
	defaultQuotaBytes int64,
) (*domainconversation.StorageQuota, error) {
	var quota models.UserStorageQuota
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		item, innerErr := getOrInitQuotaForUpdate(tx, userID, defaultQuotaBytes)
		if innerErr != nil {
			return innerErr
		}
		quota = *item
		return nil
	})
	if err != nil {
		return nil, translateError(err)
	}
	result := toStorageQuotaDomain(quota)
	return &result, nil
}

func getOrInitQuotaForUpdate(tx *gorm.DB, userID uint, defaultQuotaBytes int64) (*models.UserStorageQuota, error) {
	var quota models.UserStorageQuota
	query := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("user_id = ?", userID).
		Limit(1).
		Find(&quota)
	if query.Error != nil {
		return nil, query.Error
	}
	if query.RowsAffected == 0 {
		if defaultQuotaBytes < 0 {
			defaultQuotaBytes = 0
		}
		quota = models.UserStorageQuota{
			UserID:        userID,
			QuotaBytes:    defaultQuotaBytes,
			UsedBytes:     0,
			ReservedBytes: 0,
		}
		if err := tx.Select("UserID", "QuotaBytes", "UsedBytes", "ReservedBytes").Create(&quota).Error; err != nil {
			return nil, translateError(err)
		}
	} else {
		if defaultQuotaBytes < 0 {
			defaultQuotaBytes = 0
		}
		if quota.QuotaBytes != defaultQuotaBytes {
			if err := tx.Model(&models.UserStorageQuota{}).
				Where("id = ?", quota.ID).
				Update("quota_bytes", defaultQuotaBytes).Error; err != nil {
				return nil, translateError(err)
			}
			quota.QuotaBytes = defaultQuotaBytes
		}
	}
	return &quota, nil
}

// GetFileObjectsByInternalIDs 按内部主键 ID 批量查询文件对象。
func (r *Repo) GetFileObjectsByInternalIDs(ctx context.Context, userID uint, ids []uint) ([]domainconversation.FileObject, error) {
	items := make([]models.FileObject, 0)
	if len(ids) == 0 {
		return []domainconversation.FileObject{}, nil
	}
	if err := r.db.WithContext(ctx).
		Where("user_id = ? AND id IN ?", userID, ids).
		Find(&items).Error; err != nil {
		return nil, translateError(err)
	}
	return toFileObjectDomains(items), nil
}

func ensureFileObjectUnreferencedByActiveConversations(tx *gorm.DB, userID uint, fileID string) error {
	var activeReferences int64
	if err := tx.Table("chat_attachments AS a").
		Joins("JOIN chat_conversations AS c ON c.id = a.conversation_id AND c.user_id = a.user_id AND c.deleted_at IS NULL").
		Where("a.user_id = ? AND a.file_id = ? AND a.status <> ?", userID, fileID, "deleted").
		Count(&activeReferences).Error; err != nil {
		return translateError(err)
	}
	if activeReferences > 0 {
		return repository.ErrConflict
	}
	return nil
}

func ensureFileObjectUnreferencedByUserAvatars(tx *gorm.DB, fileID string) error {
	var activeReferences int64
	if err := tx.Model(&models.User{}).
		Where("avatar_url LIKE 'file:%' AND avatar_url = ?", domainuser.BuildFileAvatarURL(fileID)).
		Count(&activeReferences).Error; err != nil {
		return translateError(err)
	}
	if activeReferences > 0 {
		return repository.ErrConflict
	}
	return nil
}

func ensureFileObjectUnreferencedByKnowledgeBases(tx *gorm.DB, fileObjectID uint) error {
	var activeReferences int64
	if err := tx.Model(&models.KnowledgeBaseFile{}).
		Where("file_object_id = ?", fileObjectID).
		Count(&activeReferences).Error; err != nil {
		return translateError(err)
	}
	if activeReferences > 0 {
		return repository.ErrConflict
	}
	return nil
}
