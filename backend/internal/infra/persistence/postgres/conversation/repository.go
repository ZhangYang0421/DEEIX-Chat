package conversation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	domainconversation "github.com/DEEIX-AI/DEEIX-Chat/backend/internal/domain/conversation"
	domainuser "github.com/DEEIX-AI/DEEIX-Chat/backend/internal/domain/user"
	"github.com/DEEIX-AI/DEEIX-Chat/backend/internal/infra/persistence/dberror"
	models "github.com/DEEIX-AI/DEEIX-Chat/backend/internal/infra/persistence/models"
	"github.com/DEEIX-AI/DEEIX-Chat/backend/internal/repository"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	defaultMessageQueryLimit = 20
	maxMessageQueryLimit     = 1000
	maxAncestorQueryDepth    = 2000
)

// translateError 将 gorm 底层错误统一映射为仓储语义错误。
func translateError(err error) error {
	if err == nil {
		return nil
	}
	if dberror.IsRecordNotFound(err) {
		return repository.ErrNotFound
	}
	if dberror.IsUniqueConstraint(err) {
		return repository.ErrDuplicate
	}
	return err
}

func truncateText(value string, maxChars int) string {
	if maxChars <= 0 {
		return value
	}
	runes := []rune(value)
	if len(runes) <= maxChars {
		return value
	}
	return string(runes[:maxChars])
}

// Repo 聚合会话域数据访问。
type Repo struct {
	db *gorm.DB
}

// NewRepo 创建仓储。
func NewRepo(db *gorm.DB) *Repo {
	return &Repo{db: db}
}

func (r *Repo) sqliteDialect() bool {
	return r != nil && r.db != nil && r.db.Dialector != nil && r.db.Dialector.Name() == "sqlite"
}

func (r *Repo) trimFunctionName() string {
	if r.sqliteDialect() {
		return "trim"
	}
	return "btrim"
}

// CreateConversation 创建会话。
func (r *Repo) CreateConversation(ctx context.Context, item *domainconversation.Conversation) error {
	entity := toConversationModel(item)
	if err := r.db.WithContext(ctx).Create(&entity).Error; err != nil {
		return translateError(err)
	}
	*item = toConversationDomain(entity)
	return nil
}

// ListConversationsByUser 分页查询用户会话。
func (r *Repo) ListConversationsByUser(
	ctx context.Context,
	userID uint,
	offset int,
	limit int,
	statusFilter string,
	starredFilter string,
	shareFilter string,
	projectFilter string,
	searchQuery string,
) ([]domainconversation.Conversation, int64, error) {
	items := make([]models.Conversation, 0)
	var total int64
	query := r.db.WithContext(ctx).Model(&models.Conversation{}).Where("user_id = ?", userID)

	switch statusFilter {
	case "archived":
		query = query.Where("status = ?", "archived")
	case "all":
		// 保留全部状态。
	default:
		query = query.Where("status <> ?", "archived")
	}

	switch starredFilter {
	case "starred":
		query = query.Where("is_starred = ?", true)
	case "unstarred":
		query = query.Where("is_starred = ?", false)
	}

	activeShareExistsSQL := `EXISTS (
		SELECT 1
		FROM chat_conversation_shares AS shares
		WHERE shares.conversation_id = chat_conversations.id
			AND shares.user_id = chat_conversations.user_id
			AND shares.status = ?
	)`
	switch shareFilter {
	case "shared":
		query = query.Where(activeShareExistsSQL, "active")
	case "unshared":
		query = query.Where("NOT "+activeShareExistsSQL, "active")
	}

	switch normalizedProjectFilter := strings.TrimSpace(projectFilter); normalizedProjectFilter {
	case "", "all":
		// 保留全部项目归属。
	case "unassigned":
		query = query.Where("project_id IS NULL")
	default:
		project, err := r.GetConversationProjectByPublicID(ctx, userID, normalizedProjectFilter)
		if err != nil {
			if errors.Is(err, repository.ErrNotFound) {
				return []domainconversation.Conversation{}, 0, nil
			}
			return nil, 0, err
		}
		query = query.Where("project_id = ?", project.ID)
	}

	query = applyConversationSearchFilter(query, searchQuery)

	if err := query.Count(&total).Error; err != nil {
		return nil, 0, translateError(err)
	}
	orderedQuery := query.Session(&gorm.Session{})
	if starredFilter == "starred" {
		orderedQuery = orderedQuery.
			Order("starred_at DESC").
			Order("id DESC")
	} else {
		orderedQuery = orderedQuery.
			Order("updated_at DESC").
			Order("id DESC")
	}
	if err := orderedQuery.Offset(offset).
		Limit(limit).
		Find(&items).Error; err != nil {
		return nil, 0, translateError(err)
	}
	results := toConversationDomains(items)
	if err := r.hydrateConversationShareSummaries(ctx, results); err != nil {
		return nil, 0, err
	}
	if err := r.hydrateConversationProjectSummaries(ctx, results); err != nil {
		return nil, 0, err
	}
	return results, total, nil
}

// ListConversationsForSearch 返回搜索页所需的会话窗口，不执行精确总数统计。
func (r *Repo) ListConversationsForSearch(
	ctx context.Context,
	userID uint,
	offset int,
	limit int,
	searchQuery string,
) ([]domainconversation.Conversation, error) {
	items := make([]models.Conversation, 0)
	query := r.db.WithContext(ctx).
		Model(&models.Conversation{}).
		Where("user_id = ?", userID)
	query = applyConversationSearchFilter(query, searchQuery)
	if err := query.
		Order("updated_at DESC").
		Order("id DESC").
		Offset(offset).
		Limit(limit).
		Find(&items).Error; err != nil {
		return nil, translateError(err)
	}

	results := toConversationDomains(items)
	if err := r.hydrateConversationShareSummaries(ctx, results); err != nil {
		return nil, err
	}
	if err := r.hydrateConversationProjectSummaries(ctx, results); err != nil {
		return nil, err
	}
	return results, nil
}

func applyConversationSearchFilter(query *gorm.DB, searchQuery string) *gorm.DB {
	keyword := strings.TrimSpace(searchQuery)
	if keyword == "" {
		return query
	}

	like := conversationSearchLikePattern(keyword)
	return query.Where(
		`(LOWER(title) LIKE ? ESCAPE '!'
			OR LOWER(public_id) LIKE ? ESCAPE '!'
			OR LOWER(labels_json) LIKE ? ESCAPE '!'
			OR LOWER(model) LIKE ? ESCAPE '!'
			OR LOWER(provider) LIKE ? ESCAPE '!'
			OR EXISTS (
				SELECT 1
				FROM chat_conversation_projects AS projects
				WHERE projects.id = chat_conversations.project_id
					AND projects.user_id = chat_conversations.user_id
					AND projects.deleted_at IS NULL
					AND (
						LOWER(projects.name) LIKE ? ESCAPE '!'
						OR LOWER(projects.public_id) LIKE ? ESCAPE '!'
						OR LOWER(projects.description) LIKE ? ESCAPE '!'
					)
			)
			OR EXISTS (
				SELECT 1
				FROM chat_messages AS messages
				WHERE messages.conversation_id = chat_conversations.id
					AND messages.user_id = chat_conversations.user_id
					AND messages.deleted_at IS NULL
					AND messages.role IN ('user', 'assistant')
					AND LOWER(messages.content) LIKE ? ESCAPE '!'
			))`,
		like,
		like,
		like,
		like,
		like,
		like,
		like,
		like,
		like,
	)
}

func conversationSearchLikePattern(searchQuery string) string {
	keyword := strings.ToLower(strings.TrimSpace(searchQuery))
	keyword = strings.NewReplacer(
		"!", "!!",
		"%", "!%",
		"_", "!_",
	).Replace(keyword)
	return "%" + keyword + "%"
}

func (r *Repo) hydrateConversationShareSummaries(ctx context.Context, items []domainconversation.Conversation) error {
	if len(items) == 0 {
		return nil
	}
	conversationIDs := make([]uint, 0, len(items))
	for _, item := range items {
		conversationIDs = append(conversationIDs, item.ID)
	}
	shares := make([]models.ConversationShare, 0)
	if err := r.db.WithContext(ctx).
		Where("conversation_id IN ?", conversationIDs).
		Order("updated_at DESC").
		Order("id DESC").
		Find(&shares).Error; err != nil {
		return translateError(err)
	}
	latestByConversationID := make(map[uint]models.ConversationShare, len(shares))
	for _, share := range shares {
		if _, exists := latestByConversationID[share.ConversationID]; exists {
			continue
		}
		latestByConversationID[share.ConversationID] = share
	}
	for index := range items {
		share, ok := latestByConversationID[items[index].ID]
		if !ok {
			items[index].ShareStatus = "none"
			continue
		}
		items[index].ShareStatus = strings.TrimSpace(share.Status)
		if items[index].ShareStatus == "" {
			items[index].ShareStatus = "none"
		}
		if items[index].ShareStatus == "active" {
			items[index].ShareID = share.ShareID
			sharedAt := share.CreatedAt
			items[index].SharedAt = &sharedAt
			items[index].LastShareAccessedAt = share.LastAccessedAt
		}
	}
	return nil
}

func (r *Repo) hydrateConversationShareSummary(ctx context.Context, item *domainconversation.Conversation) error {
	if item == nil {
		return nil
	}
	items := []domainconversation.Conversation{*item}
	if err := r.hydrateConversationShareSummaries(ctx, items); err != nil {
		return err
	}
	*item = items[0]
	return nil
}

func (r *Repo) hydrateConversationProjectSummaries(ctx context.Context, items []domainconversation.Conversation) error {
	if len(items) == 0 {
		return nil
	}
	projectIDs := make([]uint, 0, len(items))
	seen := make(map[uint]struct{}, len(items))
	for _, item := range items {
		if item.ProjectID == nil || *item.ProjectID == 0 {
			continue
		}
		if _, exists := seen[*item.ProjectID]; exists {
			continue
		}
		seen[*item.ProjectID] = struct{}{}
		projectIDs = append(projectIDs, *item.ProjectID)
	}
	if len(projectIDs) == 0 {
		return nil
	}
	projects := make([]models.ConversationProject, 0, len(projectIDs))
	if err := r.db.WithContext(ctx).
		Where("id IN ?", projectIDs).
		Find(&projects).Error; err != nil {
		return translateError(err)
	}
	byID := make(map[uint]models.ConversationProject, len(projects))
	for _, project := range projects {
		byID[project.ID] = project
	}
	for index := range items {
		if items[index].ProjectID == nil {
			continue
		}
		project, ok := byID[*items[index].ProjectID]
		if !ok {
			continue
		}
		items[index].ProjectPublicID = project.PublicID
		items[index].ProjectName = project.Name
		items[index].ProjectSystemPrompt = project.SystemPrompt
	}
	return nil
}

func (r *Repo) hydrateConversationProjectSummary(ctx context.Context, item *domainconversation.Conversation) error {
	if item == nil {
		return nil
	}
	items := []domainconversation.Conversation{*item}
	if err := r.hydrateConversationProjectSummaries(ctx, items); err != nil {
		return err
	}
	*item = items[0]
	return nil
}

// GetConversationByUser 查询归属用户会话。
func (r *Repo) GetConversationByUser(ctx context.Context, conversationID uint, userID uint) (*domainconversation.Conversation, error) {
	var item models.Conversation
	if err := r.db.WithContext(ctx).
		Where("id = ? AND user_id = ?", conversationID, userID).
		First(&item).Error; err != nil {
		return nil, translateError(err)
	}
	result := toConversationDomain(item)
	if err := r.hydrateConversationShareSummary(ctx, &result); err != nil {
		return nil, err
	}
	if err := r.hydrateConversationProjectSummary(ctx, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// GetConversationByPublicID 查询归属用户的公开会话 ID。
func (r *Repo) GetConversationByPublicID(ctx context.Context, publicID string, userID uint) (*domainconversation.Conversation, error) {
	var item models.Conversation
	if err := r.db.WithContext(ctx).
		Where("public_id = ? AND user_id = ?", publicID, userID).
		First(&item).Error; err != nil {
		return nil, translateError(err)
	}
	result := toConversationDomain(item)
	if err := r.hydrateConversationShareSummary(ctx, &result); err != nil {
		return nil, err
	}
	if err := r.hydrateConversationProjectSummary(ctx, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// GetActiveConversationShareByConversation 查询会话当前有效分享。
func (r *Repo) GetActiveConversationShareByConversation(ctx context.Context, userID uint, conversationID uint) (*domainconversation.ConversationShare, error) {
	var item models.ConversationShare
	if err := r.db.WithContext(ctx).
		Where("user_id = ? AND conversation_id = ? AND status = ?", userID, conversationID, "active").
		Order("updated_at DESC").
		Order("id DESC").
		First(&item).Error; err != nil {
		return nil, translateError(err)
	}
	result := toConversationShareDomain(item)
	return &result, nil
}

// GetLatestConversationShareByConversation 查询会话最近一次分享记录。
func (r *Repo) GetLatestConversationShareByConversation(ctx context.Context, userID uint, conversationID uint) (*domainconversation.ConversationShare, error) {
	var item models.ConversationShare
	if err := r.db.WithContext(ctx).
		Where("user_id = ? AND conversation_id = ?", userID, conversationID).
		Order("updated_at DESC").
		Order("id DESC").
		First(&item).Error; err != nil {
		return nil, translateError(err)
	}
	result := toConversationShareDomain(item)
	return &result, nil
}

// GetActiveConversationShareByShareID 查询公开分享与未删除原会话。
func (r *Repo) GetActiveConversationShareByShareID(ctx context.Context, shareID string) (*domainconversation.ConversationShare, *domainconversation.Conversation, error) {
	var share models.ConversationShare
	if err := r.db.WithContext(ctx).
		Where("share_id = ? AND status = ?", shareID, "active").
		First(&share).Error; err != nil {
		return nil, nil, translateError(err)
	}
	var conversation models.Conversation
	if err := r.db.WithContext(ctx).
		Where("id = ? AND user_id = ?", share.ConversationID, share.UserID).
		First(&conversation).Error; err != nil {
		return nil, nil, translateError(err)
	}
	shareDomain := toConversationShareDomain(share)
	conversationDomain := toConversationDomain(conversation)
	return &shareDomain, &conversationDomain, nil
}

// CreateConversationShare 创建会话公开分享快照。
func (r *Repo) CreateConversationShare(ctx context.Context, item *domainconversation.ConversationShare) error {
	entity := toConversationShareModel(item)
	if err := r.db.WithContext(ctx).Create(&entity).Error; err != nil {
		return translateError(err)
	}
	*item = toConversationShareDomain(entity)
	return nil
}

// ReplaceActiveConversationShare 在一个事务内撤销旧分享并创建新快照。
func (r *Repo) ReplaceActiveConversationShare(ctx context.Context, item *domainconversation.ConversationShare) error {
	if item == nil {
		return nil
	}
	var created models.ConversationShare
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		now := time.Now().UTC()
		if err := tx.Model(&models.ConversationShare{}).
			Where("user_id = ? AND conversation_id = ? AND status = ?", item.UserID, item.ConversationID, "active").
			Updates(map[string]interface{}{
				"status":     "revoked",
				"revoked_at": now,
				"updated_at": now,
			}).Error; err != nil {
			return translateError(err)
		}
		created = toConversationShareModel(item)
		if err := tx.Create(&created).Error; err != nil {
			return translateError(err)
		}
		return nil
	})
	if err != nil {
		return translateError(err)
	}
	*item = toConversationShareDomain(created)
	return nil
}

// RevokeActiveConversationShares 撤销会话当前有效分享。
func (r *Repo) RevokeActiveConversationShares(ctx context.Context, userID uint, conversationIDs []uint) error {
	if len(conversationIDs) == 0 {
		return nil
	}
	now := time.Now().UTC()
	return translateError(r.db.WithContext(ctx).
		Model(&models.ConversationShare{}).
		Where("user_id = ? AND conversation_id IN ? AND status = ?", userID, conversationIDs, "active").
		Updates(map[string]interface{}{
			"status":     "revoked",
			"revoked_at": now,
			"updated_at": now,
		}).Error)
}

// TouchConversationShareAccess 记录公开分享访问时间。
func (r *Repo) TouchConversationShareAccess(ctx context.Context, shareID string, accessedAt time.Time) error {
	return translateError(r.db.WithContext(ctx).
		Model(&models.ConversationShare{}).
		Where("share_id = ? AND status = ?", shareID, "active").
		Update("last_accessed_at", accessedAt).
		Error)
}

// UpdateConversationTitleByPublicID 更新会话标题。
func (r *Repo) UpdateConversationTitleByPublicID(
	ctx context.Context,
	userID uint,
	publicID string,
	title string,
) (*domainconversation.Conversation, error) {
	result := r.db.WithContext(ctx).
		Model(&models.Conversation{}).
		Where("user_id = ? AND public_id = ?", userID, publicID).
		Update("title", title)
	if result.Error != nil {
		return nil, translateError(result.Error)
	}
	if result.RowsAffected == 0 {
		return nil, repository.ErrNotFound
	}
	return r.GetConversationByPublicID(ctx, publicID, userID)
}

// UpdateConversationMetadata 更新自动生成的会话元数据。
func (r *Repo) UpdateConversationMetadata(ctx context.Context, conversationID uint, patch repository.ConversationMetadataPatch) (*domainconversation.Conversation, error) {
	updates := map[string]interface{}{}
	if strings.TrimSpace(patch.Title) != "" {
		replaceable := []string{"new chat", "新对话"}
		for _, item := range patch.ReplaceableTitles {
			value := strings.TrimSpace(strings.ToLower(item))
			if value != "" {
				replaceable = append(replaceable, value)
			}
		}
		updates["title"] = gorm.Expr(
			fmt.Sprintf("CASE WHEN lower(%s(title)) IN ? THEN ? ELSE title END", r.trimFunctionName()),
			replaceable,
			strings.TrimSpace(patch.Title),
		)
	}
	if len(updates) == 0 {
		var current models.Conversation
		if err := r.db.WithContext(ctx).Where("id = ?", conversationID).First(&current).Error; err != nil {
			return nil, translateError(err)
		}
		result := toConversationDomain(current)
		return &result, nil
	}
	result := r.db.WithContext(ctx).
		Model(&models.Conversation{}).
		Where("id = ?", conversationID).
		Updates(updates)
	if result.Error != nil {
		return nil, translateError(result.Error)
	}
	var current models.Conversation
	if err := r.db.WithContext(ctx).Where("id = ?", conversationID).First(&current).Error; err != nil {
		return nil, translateError(err)
	}
	updated := toConversationDomain(current)
	return &updated, nil
}

// UpdateConversationLabelsByPublicID 按用户归属更新手动管理的会话标签。
func (r *Repo) UpdateConversationLabelsByPublicID(
	ctx context.Context,
	userID uint,
	publicID string,
	labelsJSON string,
) (*domainconversation.Conversation, error) {
	result := r.db.WithContext(ctx).
		Model(&models.Conversation{}).
		Where("user_id = ? AND public_id = ?", userID, publicID).
		Updates(map[string]interface{}{
			"labels_json":             strings.TrimSpace(labelsJSON),
			"labels_manually_managed": true,
		})
	if result.Error != nil {
		return nil, translateError(result.Error)
	}
	if result.RowsAffected == 0 {
		return nil, repository.ErrNotFound
	}
	return r.GetConversationByPublicID(ctx, publicID, userID)
}

// SetGeneratedConversationLabelsIfEligible 仅为空且未被用户接管时写入自动标签。
func (r *Repo) SetGeneratedConversationLabelsIfEligible(
	ctx context.Context,
	conversationID uint,
	labelsJSON string,
) (*domainconversation.Conversation, bool, error) {
	result := r.db.WithContext(ctx).
		Model(&models.Conversation{}).
		Where("id = ?", conversationID).
		Where(
			fmt.Sprintf("labels_manually_managed = ? AND lower(%s(labels_json)) IN ?", r.trimFunctionName()),
			false,
			[]string{"", "null", "[]"},
		).
		Update("labels_json", strings.TrimSpace(labelsJSON))
	if result.Error != nil {
		return nil, false, translateError(result.Error)
	}

	var current models.Conversation
	if err := r.db.WithContext(ctx).Where("id = ?", conversationID).First(&current).Error; err != nil {
		return nil, false, translateError(err)
	}
	updated := toConversationDomain(current)
	return &updated, result.RowsAffected > 0, nil
}

// UpdateConversationStarByPublicID 更新会话星标状态。
func (r *Repo) UpdateConversationStarByPublicID(
	ctx context.Context,
	userID uint,
	publicID string,
	starred bool,
) (*domainconversation.Conversation, error) {
	current, err := r.GetConversationByPublicID(ctx, publicID, userID)
	if err != nil {
		return nil, translateError(err)
	}
	if current.IsStarred == starred {
		return current, nil
	}

	var starredAt interface{}
	if starred {
		now := time.Now().UTC()
		starredAt = &now
	} else {
		starredAt = nil
	}

	result := r.db.WithContext(ctx).
		Model(&models.Conversation{}).
		Where("user_id = ? AND public_id = ?", userID, publicID).
		UpdateColumns(map[string]interface{}{
			"is_starred": starred,
			"starred_at": starredAt,
		})
	if result.Error != nil {
		return nil, translateError(result.Error)
	}
	if result.RowsAffected == 0 {
		return nil, repository.ErrNotFound
	}
	return r.GetConversationByPublicID(ctx, publicID, userID)
}

// UpdateConversationArchiveByPublicID 更新会话归档状态。
func (r *Repo) UpdateConversationArchiveByPublicID(
	ctx context.Context,
	userID uint,
	publicID string,
	archived bool,
) (*domainconversation.Conversation, error) {
	nextStatus := "active"
	if archived {
		nextStatus = "archived"
	}
	result := r.db.WithContext(ctx).
		Model(&models.Conversation{}).
		Where("user_id = ? AND public_id = ?", userID, publicID).
		Update("status", nextStatus)
	if result.Error != nil {
		return nil, translateError(result.Error)
	}
	if result.RowsAffected == 0 {
		return nil, repository.ErrNotFound
	}
	return r.GetConversationByPublicID(ctx, publicID, userID)
}

// DeleteConversationByPublicID 删除会话（软删除），并可返回仅被该会话引用的文件 ID。
func (r *Repo) DeleteConversationByPublicID(ctx context.Context, userID uint, publicID string, deleteFiles bool) ([]string, error) {
	normalizedPublicID := strings.TrimSpace(publicID)
	cleanupFileIDs := make([]string, 0)
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var item models.Conversation
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("user_id = ? AND public_id = ?", userID, normalizedPublicID).
			First(&item).Error; err != nil {
			return translateError(err)
		}
		if err := tx.Delete(&item).Error; err != nil {
			return translateError(err)
		}
		if !deleteFiles {
			return nil
		}
		// 候选文件必须在会话软删除后计算，避免仍被其他活跃会话引用的文件被误删。
		fileIDs, err := listConversationFileCleanupCandidates(tx, userID, []uint{item.ID})
		if err != nil {
			return err
		}
		cleanupFileIDs = fileIDs
		return nil
	})
	if err != nil {
		return nil, translateError(err)
	}
	return cleanupFileIDs, nil
}

type conversationFileCleanupCandidate struct {
	FileID string `gorm:"column:file_id"`
}

// listConversationFileCleanupCandidates 返回仅被指定会话集合引用、且仍处于 active 状态的文件 ID。
func listConversationFileCleanupCandidates(tx *gorm.DB, userID uint, conversationIDs []uint) ([]string, error) {
	if len(conversationIDs) == 0 {
		return nil, nil
	}
	activeReferenceQuery := tx.
		Table("chat_attachments AS other_a").
		Select("1").
		Joins("JOIN chat_conversations AS other_c ON other_c.id = other_a.conversation_id AND other_c.user_id = other_a.user_id AND other_c.deleted_at IS NULL").
		Where("other_a.user_id = a.user_id AND other_a.file_id = a.file_id AND other_a.status <> ?", "deleted")

	rows := make([]conversationFileCleanupCandidate, 0)
	if err := tx.
		Table("chat_attachments AS a").
		Select("DISTINCT a.file_id AS file_id").
		Joins("JOIN file_objects AS fo ON fo.user_id = a.user_id AND fo.file_id = a.file_id AND fo.status = ? AND fo.deleted_at IS NULL", "active").
		Where("a.user_id = ? AND a.conversation_id IN ? AND a.status <> ? AND a.file_id <> ''", userID, conversationIDs, "deleted").
		Where("NOT EXISTS (?)", activeReferenceQuery).
		Order("a.file_id ASC").
		Scan(&rows).Error; err != nil {
		return nil, translateError(err)
	}
	fileIDs := make([]string, 0, len(rows))
	for _, row := range rows {
		if strings.TrimSpace(row.FileID) != "" {
			fileIDs = append(fileIDs, row.FileID)
		}
	}
	return fileIDs, nil
}

func lockActiveFileObjectsForAttachments(tx *gorm.DB, userID uint, attachments []domainconversation.Attachment) error {
	fileIDs := make([]string, 0, len(attachments))
	seen := make(map[string]struct{}, len(attachments))
	for i := range attachments {
		fileID := strings.TrimSpace(attachments[i].FileID)
		if fileID == "" {
			continue
		}
		attachmentUserID := attachments[i].UserID
		if userID == 0 {
			userID = attachmentUserID
		}
		if attachmentUserID != 0 && attachmentUserID != userID {
			return repository.ErrInvalidInput
		}
		if _, exists := seen[fileID]; exists {
			continue
		}
		seen[fileID] = struct{}{}
		fileIDs = append(fileIDs, fileID)
	}
	if len(fileIDs) == 0 {
		return nil
	}
	if userID == 0 {
		return repository.ErrInvalidInput
	}

	lockedIDs := make([]uint, 0, len(fileIDs))
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Model(&models.FileObject{}).
		Where("user_id = ? AND status = ? AND file_id IN ?", userID, "active", fileIDs).
		Pluck("id", &lockedIDs).Error; err != nil {
		return translateError(err)
	}
	if len(lockedIDs) != len(fileIDs) {
		return repository.ErrNotFound
	}
	return nil
}

// GetUserByID 按 ID 查询用户。
func (r *Repo) GetUserByID(ctx context.Context, userID uint) (*domainuser.User, error) {
	var item models.User
	if err := r.db.WithContext(ctx).Where("id = ?", userID).First(&item).Error; err != nil {
		return nil, translateError(err)
	}
	result := toUserDomain(item)
	return &result, nil
}

// IncrementMessageCount 增加消息计数。
func (r *Repo) IncrementMessageCount(ctx context.Context, conversationID uint, delta int) error {
	return translateError(r.db.WithContext(ctx).
		Model(&models.Conversation{}).
		Where("id = ?", conversationID).
		Update("message_count", gorm.Expr("message_count + ?", delta)).
		Error)
}

// UpdateConversationCompactedAt 更新会话最近压缩时间。
func (r *Repo) UpdateConversationCompactedAt(ctx context.Context, conversationID uint, compactedAt time.Time) error {
	return translateError(r.db.WithContext(ctx).
		Model(&models.Conversation{}).
		Where("id = ?", conversationID).
		Updates(map[string]interface{}{
			"last_compacted_at": compactedAt,
		}).
		Error)
}

// UpdateConversationLastResponseID 更新会话最近响应 ID。
func (r *Repo) UpdateConversationLastResponseID(ctx context.Context, conversationID uint, responseID string) error {
	updates := map[string]interface{}{"last_response_id": responseID}
	if strings.TrimSpace(responseID) == "" {
		updates["last_prompt_fingerprint"] = ""
	}
	return translateError(r.db.WithContext(ctx).
		Model(&models.Conversation{}).
		Where("id = ?", conversationID).
		Updates(updates).
		Error)
}

// UpdateConversationStatefulResponse 同步更新最新响应 ID 与对应的本地上下文状态指纹。
func (r *Repo) UpdateConversationStatefulResponse(ctx context.Context, conversationID uint, responseID string, promptFingerprint string) error {
	return translateError(r.db.WithContext(ctx).
		Model(&models.Conversation{}).
		Where("id = ?", conversationID).
		Updates(map[string]interface{}{
			"last_response_id":        responseID,
			"last_prompt_fingerprint": promptFingerprint,
		}).
		Error)
}

// UpdateConversationModel 更新会话当前使用模型与提供商。
func (r *Repo) UpdateConversationModel(ctx context.Context, conversationID uint, platformModelName string, provider string) error {
	return translateError(r.db.WithContext(ctx).
		Model(&models.Conversation{}).
		Where("id = ?", conversationID).
		Updates(map[string]interface{}{
			"model":    platformModelName,
			"provider": provider,
		}).
		Error)
}

// ListAllConversationsAfterID 按主键游标分页列出会话（管理员导出用）。
func (r *Repo) ListAllConversationsAfterID(ctx context.Context, afterID uint, limit int) ([]domainconversation.Conversation, error) {
	var rows []models.Conversation
	query := r.db.WithContext(ctx).Order("id ASC").Limit(limit)
	if afterID > 0 {
		query = query.Where("id > ?", afterID)
	}
	if err := query.Find(&rows).Error; err != nil {
		return nil, translateError(err)
	}
	return toConversationDomains(rows), nil
}

// ListUserConversationsAfterID 按主键游标分页列出指定用户的会话。
func (r *Repo) ListUserConversationsAfterID(ctx context.Context, userID uint, afterID uint, limit int) ([]domainconversation.Conversation, error) {
	var rows []models.Conversation
	query := r.db.WithContext(ctx).Where("user_id = ?", userID).Order("id ASC").Limit(limit)
	if afterID > 0 {
		query = query.Where("id > ?", afterID)
	}
	if err := query.Find(&rows).Error; err != nil {
		return nil, translateError(err)
	}
	return toConversationDomains(rows), nil
}

// CreateMessage 创建消息。
func (r *Repo) CreateMessage(ctx context.Context, item *domainconversation.Message) error {
	attachmentSnapshot := item.Attachments
	entity := toMessageModel(item)
	if err := r.db.WithContext(ctx).Create(&entity).Error; err != nil {
		return translateError(err)
	}
	*item = toMessageDomain(entity)
	item.Attachments = attachmentSnapshot
	return nil
}

// CreateAssistantBranchMessage 原子创建 assistant 分支消息并递增会话消息数。
func (r *Repo) CreateAssistantBranchMessage(ctx context.Context, assistantMessage *domainconversation.Message) error {
	if assistantMessage == nil || assistantMessage.ParentMessageID == nil {
		return repository.ErrInvalidInput
	}
	attachmentSnapshot := assistantMessage.Attachments
	return translateError(r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		entity := toMessageModel(assistantMessage)
		if err := tx.Create(&entity).Error; err != nil {
			return err
		}
		*assistantMessage = toMessageDomain(entity)
		assistantMessage.Attachments = attachmentSnapshot

		result := tx.Model(&models.Conversation{}).
			Where("id = ?", assistantMessage.ConversationID).
			Update("message_count", gorm.Expr("message_count + ?", 1))
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return repository.ErrNotFound
		}
		return nil
	}))
}

// CreateMessagePairWithUserAttachments 原子创建用户消息、助手占位消息、用户附件并递增会话消息数。
func (r *Repo) CreateMessagePairWithUserAttachments(
	ctx context.Context,
	userMessage *domainconversation.Message,
	assistantMessage *domainconversation.Message,
	userAttachments []domainconversation.Attachment,
) error {
	if userMessage == nil || assistantMessage == nil {
		return repository.ErrInvalidInput
	}
	userAttachmentSnapshot := userMessage.Attachments
	assistantAttachmentSnapshot := assistantMessage.Attachments
	return translateError(r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		userEntity := toMessageModel(userMessage)
		if err := tx.Create(&userEntity).Error; err != nil {
			return err
		}
		*userMessage = toMessageDomain(userEntity)
		userMessage.Attachments = userAttachmentSnapshot

		if len(userAttachments) > 0 {
			if err := lockActiveFileObjectsForAttachments(tx, userMessage.UserID, userAttachments); err != nil {
				return err
			}
			entities := make([]models.Attachment, 0, len(userAttachments))
			for i := range userAttachments {
				item := userAttachments[i]
				item.ConversationID = userMessage.ConversationID
				item.MessageID = userMessage.ID
				item.UserID = userMessage.UserID
				entities = append(entities, toAttachmentModel(&item))
			}
			if err := tx.Create(&entities).Error; err != nil {
				return err
			}
		}

		parentMessageID := userMessage.ID
		assistantMessage.ParentMessageID = &parentMessageID
		assistantEntity := toMessageModel(assistantMessage)
		if err := tx.Create(&assistantEntity).Error; err != nil {
			return err
		}
		*assistantMessage = toMessageDomain(assistantEntity)
		assistantMessage.Attachments = assistantAttachmentSnapshot

		result := tx.Model(&models.Conversation{}).
			Where("id = ?", userMessage.ConversationID).
			Update("message_count", gorm.Expr("message_count + ?", 2))
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return repository.ErrNotFound
		}
		return nil
	}))
}

// GetMessageByPublicID 查询归属会话的消息。
func (r *Repo) GetMessageByPublicID(
	ctx context.Context,
	conversationID uint,
	userID uint,
	publicID string,
) (*domainconversation.Message, error) {
	var item models.Message
	if err := r.db.WithContext(ctx).
		Where("conversation_id = ? AND user_id = ? AND public_id = ?", conversationID, userID, publicID).
		First(&item).Error; err != nil {
		return nil, translateError(err)
	}
	single := []models.Message{item}
	if err := r.hydrateMessageRefs(ctx, single); err != nil {
		return nil, err
	}
	if err := r.hydrateMessageAttachments(ctx, single); err != nil {
		return nil, err
	}
	item = single[0]
	result := toMessageDomain(item)
	return &result, nil
}

// GetMessageByPublicIDForUser 查询当前用户可访问的消息。
func (r *Repo) GetMessageByPublicIDForUser(ctx context.Context, userID uint, publicID string) (*domainconversation.Message, error) {
	var item models.Message
	if err := r.db.WithContext(ctx).
		Where("user_id = ? AND public_id = ?", userID, publicID).
		First(&item).Error; err != nil {
		return nil, translateError(err)
	}
	single := []models.Message{item}
	if err := r.hydrateMessageRefs(ctx, single); err != nil {
		return nil, err
	}
	if err := r.hydrateMessageAttachments(ctx, single); err != nil {
		return nil, err
	}
	item = single[0]
	result := toMessageDomain(item)
	return &result, nil
}

// UpdateMessageUsage 更新消息 token 使用量字段。
func (r *Repo) UpdateMessageUsage(
	ctx context.Context,
	messageID uint,
	inputTokens int64,
	outputTokens int64,
	cacheReadTokens int64,
	cacheWriteTokens int64,
	reasoningTokens int64,
) error {
	tokenUsage := inputTokens + cacheReadTokens + cacheWriteTokens + outputTokens + reasoningTokens
	if tokenUsage < 0 {
		tokenUsage = 0
	}
	return translateError(r.db.WithContext(ctx).
		Model(&models.Message{}).
		Where("id = ?", messageID).
		Updates(map[string]interface{}{
			"token_usage":        tokenUsage,
			"input_tokens":       inputTokens,
			"output_tokens":      outputTokens,
			"cache_read_tokens":  cacheReadTokens,
			"cache_write_tokens": cacheWriteTokens,
			"reasoning_tokens":   reasoningTokens,
		}).
		Error)
}

// UpdateMessageState 更新消息处理状态与错误信息。
func (r *Repo) UpdateMessageState(
	ctx context.Context,
	messageID uint,
	status string,
	errorCode string,
	errorMessage string,
) error {
	return translateError(r.db.WithContext(ctx).
		Model(&models.Message{}).
		Where("id = ?", messageID).
		Updates(map[string]interface{}{
			"status":        status,
			"error_code":    errorCode,
			"error_message": errorMessage,
		}).
		Error)
}

// UpdateAssistantMessageContent 更新当前用户 assistant 消息正文并标记编辑时间。
func (r *Repo) UpdateAssistantMessageContent(
	ctx context.Context,
	userID uint,
	publicID string,
	content string,
	editedAt time.Time,
) (*domainconversation.Message, error) {
	normalizedPublicID := strings.TrimSpace(publicID)
	if userID == 0 || normalizedPublicID == "" {
		return nil, repository.ErrInvalidInput
	}

	var item models.Message
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.
			Where("user_id = ? AND public_id = ? AND role = ?", userID, normalizedPublicID, "assistant").
			First(&item).Error; err != nil {
			return err
		}
		if err := tx.Model(&models.Message{}).
			Where("id = ?", item.ID).
			Updates(map[string]interface{}{
				"content":   content,
				"edited_at": editedAt,
			}).Error; err != nil {
			return err
		}
		return tx.Where("id = ?", item.ID).First(&item).Error
	})
	if err != nil {
		return nil, translateError(err)
	}

	single := []models.Message{item}
	if err = r.hydrateMessageRefs(ctx, single); err != nil {
		return nil, err
	}
	if err = r.hydrateMessageAttachments(ctx, single); err != nil {
		return nil, err
	}
	item = single[0]
	result := toMessageDomain(item)
	return &result, nil
}

// CancelPendingGenerationMessagesByRunID 将用户显式取消的 pending 回合更新为稳定终态。
func (r *Repo) CancelPendingGenerationMessagesByRunID(
	ctx context.Context,
	userID uint,
	runID string,
	errorCode string,
	errorMessage string,
) (bool, error) {
	normalizedRunID := strings.TrimSpace(runID)
	if userID == 0 || normalizedRunID == "" {
		return false, repository.ErrInvalidInput
	}
	normalizedErrorCode := strings.TrimSpace(errorCode)
	normalizedErrorMessage := truncateText(strings.TrimSpace(errorMessage), 255)
	returnedRows := int64(0)
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		userResult := tx.Model(&models.Message{}).
			Where("user_id = ? AND run_id = ? AND role = ? AND status = ?", userID, normalizedRunID, "user", "pending").
			Updates(map[string]interface{}{
				"status":        "success",
				"error_code":    "",
				"error_message": "",
			})
		if userResult.Error != nil {
			return userResult.Error
		}
		assistantResult := tx.Model(&models.Message{}).
			Where("user_id = ? AND run_id = ? AND role = ? AND status = ?", userID, normalizedRunID, "assistant", "pending").
			Updates(map[string]interface{}{
				"status":        "canceled",
				"error_code":    normalizedErrorCode,
				"error_message": normalizedErrorMessage,
			})
		if assistantResult.Error != nil {
			return assistantResult.Error
		}
		returnedRows = userResult.RowsAffected + assistantResult.RowsAffected
		return nil
	})
	if err != nil {
		return false, translateError(err)
	}
	return returnedRows > 0, nil
}

// InterruptPendingAssistantMessageByRunID 将失去活跃生成流的 pending assistant 标记为错误。
func (r *Repo) InterruptPendingAssistantMessageByRunID(
	ctx context.Context,
	userID uint,
	runID string,
	errorCode string,
	errorMessage string,
) (bool, error) {
	result := r.db.WithContext(ctx).
		Model(&models.Message{}).
		Where("user_id = ? AND run_id = ? AND role = ? AND status = ?", userID, strings.TrimSpace(runID), "assistant", "pending").
		Updates(map[string]interface{}{
			"status":        "error",
			"error_code":    strings.TrimSpace(errorCode),
			"error_message": truncateText(strings.TrimSpace(errorMessage), 255),
		})
	if result.Error != nil {
		return false, translateError(result.Error)
	}
	return result.RowsAffected > 0, nil
}

// UpdateAssistantMessageCompletion 回填 assistant 消息正文、用量与状态。
func (r *Repo) UpdateAssistantMessageCompletion(
	ctx context.Context,
	messageID uint,
	update repository.AssistantMessageCompletionUpdate,
) error {
	tokenUsage := update.InputTokens + update.CacheReadTokens + update.CacheWriteTokens + update.OutputTokens + update.ReasoningTokens
	if tokenUsage < 0 {
		tokenUsage = 0
	}
	if update.LatencyMS < 0 {
		update.LatencyMS = 0
	}
	updates := map[string]interface{}{
		"content":            update.Content,
		"reasoning_content":  update.ReasoningContent,
		"token_usage":        tokenUsage,
		"input_tokens":       update.InputTokens,
		"output_tokens":      update.OutputTokens,
		"cache_read_tokens":  update.CacheReadTokens,
		"cache_write_tokens": update.CacheWriteTokens,
		"reasoning_tokens":   update.ReasoningTokens,
		"latency_ms":         update.LatencyMS,
		"status":             update.Status,
		"error_code":         update.ErrorCode,
		"error_message":      update.ErrorMessage,
	}
	if update.KnowledgeSources != nil {
		updates["knowledge_sources_json"] = marshalMessageKnowledgeSources(update.KnowledgeSources)
	}
	return translateError(r.db.WithContext(ctx).
		Model(&models.Message{}).
		Where("id = ?", messageID).
		Updates(updates).
		Error)
}

// CompleteAssistantMessageWithAttachments 原子写入助手附件，并同步用户用量与助手完成态。
func (r *Repo) CompleteAssistantMessageWithAttachments(
	ctx context.Context,
	userMessageID uint,
	userUsage repository.MessageUsageUpdate,
	assistantMessageID uint,
	assistantCompletion repository.AssistantMessageCompletionUpdate,
	assistantAttachments []domainconversation.Attachment,
) error {
	return translateError(r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if len(assistantAttachments) > 0 {
			if err := lockActiveFileObjectsForAttachments(tx, 0, assistantAttachments); err != nil {
				return err
			}
			entities := make([]models.Attachment, 0, len(assistantAttachments))
			for i := range assistantAttachments {
				item := assistantAttachments[i]
				item.MessageID = assistantMessageID
				entities = append(entities, toAttachmentModel(&item))
			}
			if err := tx.Create(&entities).Error; err != nil {
				return err
			}
		}

		userTokenUsage := userUsage.InputTokens + userUsage.CacheReadTokens + userUsage.CacheWriteTokens + userUsage.OutputTokens + userUsage.ReasoningTokens
		if userTokenUsage < 0 {
			userTokenUsage = 0
		}
		if err := tx.Model(&models.Message{}).
			Where("id = ?", userMessageID).
			Updates(map[string]interface{}{
				"token_usage":        userTokenUsage,
				"input_tokens":       userUsage.InputTokens,
				"output_tokens":      userUsage.OutputTokens,
				"cache_read_tokens":  userUsage.CacheReadTokens,
				"cache_write_tokens": userUsage.CacheWriteTokens,
				"reasoning_tokens":   userUsage.ReasoningTokens,
			}).Error; err != nil {
			return err
		}

		assistantTokenUsage := assistantCompletion.InputTokens + assistantCompletion.CacheReadTokens + assistantCompletion.CacheWriteTokens + assistantCompletion.OutputTokens + assistantCompletion.ReasoningTokens
		if assistantTokenUsage < 0 {
			assistantTokenUsage = 0
		}
		latencyMS := assistantCompletion.LatencyMS
		if latencyMS < 0 {
			latencyMS = 0
		}
		updates := map[string]interface{}{
			"content":            assistantCompletion.Content,
			"reasoning_content":  assistantCompletion.ReasoningContent,
			"token_usage":        assistantTokenUsage,
			"input_tokens":       assistantCompletion.InputTokens,
			"output_tokens":      assistantCompletion.OutputTokens,
			"cache_read_tokens":  assistantCompletion.CacheReadTokens,
			"cache_write_tokens": assistantCompletion.CacheWriteTokens,
			"reasoning_tokens":   assistantCompletion.ReasoningTokens,
			"latency_ms":         latencyMS,
			"status":             assistantCompletion.Status,
			"error_code":         assistantCompletion.ErrorCode,
			"error_message":      assistantCompletion.ErrorMessage,
		}
		if contentType := strings.TrimSpace(assistantCompletion.ContentType); contentType != "" {
			updates["content_type"] = contentType
		}
		if assistantCompletion.KnowledgeSources != nil {
			updates["knowledge_sources_json"] = marshalMessageKnowledgeSources(assistantCompletion.KnowledgeSources)
		}
		return tx.Model(&models.Message{}).
			Where("id = ?", assistantMessageID).
			Updates(updates).Error
	}))
}

// CompleteAssistantMessageWithGeneratedAttachments 原子写入助手附件并同步助手完成态，不修改父用户消息。
func (r *Repo) CompleteAssistantMessageWithGeneratedAttachments(
	ctx context.Context,
	assistantMessageID uint,
	assistantCompletion repository.AssistantMessageCompletionUpdate,
	assistantAttachments []domainconversation.Attachment,
) error {
	return translateError(r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if len(assistantAttachments) > 0 {
			if err := lockActiveFileObjectsForAttachments(tx, 0, assistantAttachments); err != nil {
				return err
			}
			entities := make([]models.Attachment, 0, len(assistantAttachments))
			for i := range assistantAttachments {
				item := assistantAttachments[i]
				item.MessageID = assistantMessageID
				entities = append(entities, toAttachmentModel(&item))
			}
			if err := tx.Create(&entities).Error; err != nil {
				return err
			}
		}

		assistantTokenUsage := assistantCompletion.InputTokens + assistantCompletion.CacheReadTokens + assistantCompletion.CacheWriteTokens + assistantCompletion.OutputTokens + assistantCompletion.ReasoningTokens
		if assistantTokenUsage < 0 {
			assistantTokenUsage = 0
		}
		latencyMS := assistantCompletion.LatencyMS
		if latencyMS < 0 {
			latencyMS = 0
		}
		updates := map[string]interface{}{
			"content":            assistantCompletion.Content,
			"reasoning_content":  assistantCompletion.ReasoningContent,
			"token_usage":        assistantTokenUsage,
			"input_tokens":       assistantCompletion.InputTokens,
			"output_tokens":      assistantCompletion.OutputTokens,
			"cache_read_tokens":  assistantCompletion.CacheReadTokens,
			"cache_write_tokens": assistantCompletion.CacheWriteTokens,
			"reasoning_tokens":   assistantCompletion.ReasoningTokens,
			"latency_ms":         latencyMS,
			"status":             assistantCompletion.Status,
			"error_code":         assistantCompletion.ErrorCode,
			"error_message":      assistantCompletion.ErrorMessage,
		}
		if contentType := strings.TrimSpace(assistantCompletion.ContentType); contentType != "" {
			updates["content_type"] = contentType
		}
		if assistantCompletion.KnowledgeSources != nil {
			updates["knowledge_sources_json"] = marshalMessageKnowledgeSources(assistantCompletion.KnowledgeSources)
		}
		return tx.Model(&models.Message{}).
			Where("id = ?", assistantMessageID).
			Updates(updates).Error
	}))
}

// UpdateMessageBilling 回填消息计费金额与计费快照。
func (r *Repo) UpdateMessageBilling(ctx context.Context, messageID uint, billedCurrency string, billedNanousd int64, pricingSnapshot string) error {
	if billedNanousd < 0 {
		billedNanousd = 0
	}
	return translateError(r.db.WithContext(ctx).
		Model(&models.Message{}).
		Where("id = ?", messageID).
		Updates(map[string]interface{}{
			"billed_currency":  billedCurrency,
			"billed_nanousd":   billedNanousd,
			"pricing_snapshot": pricingSnapshot,
		}).
		Error)
}

// SumMessageTokens 统计会话 token 消耗总量。
func (r *Repo) SumMessageTokens(ctx context.Context, conversationID uint) (int64, error) {
	var total int64
	if err := r.db.WithContext(ctx).
		Model(&models.Message{}).
		Select("COALESCE(SUM(token_usage), 0)").
		Where("conversation_id = ?", conversationID).
		Scan(&total).Error; err != nil {
		return 0, translateError(err)
	}
	return total, nil
}

// ListMessages 查询会话消息。
func (r *Repo) ListMessages(ctx context.Context, conversationID uint, offset int, limit int) ([]domainconversation.Message, int64, error) {
	items := make([]models.Message, 0)
	var total int64

	if err := r.db.WithContext(ctx).
		Model(&models.Message{}).
		Where("conversation_id = ?", conversationID).
		Count(&total).Error; err != nil {
		return nil, 0, translateError(err)
	}
	if err := r.db.WithContext(ctx).
		Where("conversation_id = ?", conversationID).
		Order("id ASC").
		Offset(offset).
		Limit(limit).
		Find(&items).Error; err != nil {
		return nil, 0, translateError(err)
	}
	if err := r.hydrateMessageRefs(ctx, items); err != nil {
		return nil, 0, err
	}
	if err := r.hydrateMessageAttachments(ctx, items); err != nil {
		return nil, 0, err
	}
	return toMessageDomains(items), total, nil
}

// ListMessagesBeforeID 查询指定消息 ID 之前的一页会话消息（按时间升序返回）。
func (r *Repo) ListMessagesBeforeID(ctx context.Context, conversationID uint, beforeID uint, limit int) ([]domainconversation.Message, int64, error) {
	if limit <= 0 {
		limit = defaultMessageQueryLimit
	}
	if limit > maxMessageQueryLimit {
		limit = maxMessageQueryLimit
	}
	items := make([]models.Message, 0)
	var total int64

	if err := r.db.WithContext(ctx).
		Model(&models.Message{}).
		Where("conversation_id = ?", conversationID).
		Count(&total).Error; err != nil {
		return nil, 0, translateError(err)
	}

	if err := r.db.WithContext(ctx).
		Where("conversation_id = ? AND id < ?", conversationID, beforeID).
		Order("id DESC").
		Limit(limit).
		Find(&items).Error; err != nil {
		return nil, 0, translateError(err)
	}
	for left, right := 0, len(items)-1; left < right; left, right = left+1, right-1 {
		items[left], items[right] = items[right], items[left]
	}
	if err := r.hydrateMessageRefs(ctx, items); err != nil {
		return nil, 0, err
	}
	if err := r.hydrateMessageAttachments(ctx, items); err != nil {
		return nil, 0, err
	}
	return toMessageDomains(items), total, nil
}

// ListMessagesForShare 查询分享快照可公开展示的消息。
func (r *Repo) ListMessagesForShare(ctx context.Context, conversationID uint, publicIDs []string) ([]domainconversation.Message, error) {
	items := make([]models.Message, 0)
	query := r.db.WithContext(ctx).Where("conversation_id = ?", conversationID)
	if len(publicIDs) > 0 {
		query = query.Where("public_id IN ?", publicIDs)
	}
	if err := query.Order("id ASC").Find(&items).Error; err != nil {
		return nil, translateError(err)
	}
	if err := r.hydrateMessageRefs(ctx, items); err != nil {
		return nil, err
	}
	if err := r.hydrateMessageAttachments(ctx, items); err != nil {
		return nil, err
	}
	result := toMessageDomains(items)
	if len(publicIDs) == 0 {
		return result, nil
	}
	byPublicID := make(map[string]domainconversation.Message, len(result))
	for _, item := range result {
		byPublicID[item.PublicID] = item
	}
	ordered := make([]domainconversation.Message, 0, len(publicIDs))
	for _, publicID := range publicIDs {
		item, ok := byPublicID[publicID]
		if !ok {
			continue
		}
		ordered = append(ordered, item)
	}
	return ordered, nil
}

// ListAllMessages 查询会话全部消息。
func (r *Repo) ListAllMessages(ctx context.Context, conversationID uint) ([]domainconversation.Message, error) {
	items := make([]models.Message, 0)
	if err := r.db.WithContext(ctx).
		Where("conversation_id = ?", conversationID).
		Order("id ASC").
		Find(&items).Error; err != nil {
		return nil, translateError(err)
	}
	if err := r.hydrateMessageRefs(ctx, items); err != nil {
		return nil, err
	}
	if err := r.hydrateMessageAttachments(ctx, items); err != nil {
		return nil, err
	}
	return toMessageDomains(items), nil
}

// UpsertMessageFeedback 写入或更新消息反馈。
func (r *Repo) UpsertMessageFeedback(ctx context.Context, item *domainconversation.MessageFeedback) error {
	entity := toMessageFeedbackModel(item)
	return translateError(r.db.WithContext(ctx).
		Clauses(clause.OnConflict{
			Columns: []clause.Column{
				{Name: "user_id"},
				{Name: "message_id"},
			},
			DoUpdates: clause.AssignmentColumns([]string{
				"conversation_id",
				"feedback",
				"updated_at",
			}),
		}).
		Create(&entity).Error)
}

// DeleteMessageFeedback 删除用户对消息的反馈。
func (r *Repo) DeleteMessageFeedback(ctx context.Context, userID uint, messageID uint) error {
	return translateError(r.db.WithContext(ctx).
		Where("user_id = ? AND message_id = ?", userID, messageID).
		Delete(&models.ConversationMessageFeedback{}).Error)
}

// GetUserMessageFeedbackMap 查询用户对消息列表的反馈映射。
func (r *Repo) GetUserMessageFeedbackMap(
	ctx context.Context,
	userID uint,
	messageIDs []uint,
) (map[uint]string, error) {
	result := make(map[uint]string, len(messageIDs))
	if len(messageIDs) == 0 {
		return result, nil
	}

	items := make([]models.ConversationMessageFeedback, 0, len(messageIDs))
	if err := r.db.WithContext(ctx).
		Where("user_id = ? AND message_id IN ?", userID, messageIDs).
		Find(&items).Error; err != nil {
		return nil, translateError(err)
	}
	for _, item := range items {
		result[item.MessageID] = item.Feedback
	}
	return result, nil
}

type messageFeedbackAggregateRow struct {
	MessageID uint   `gorm:"column:message_id"`
	Feedback  string `gorm:"column:feedback"`
	Count     int64  `gorm:"column:count"`
}

// GetMessageFeedbackCounts 查询消息列表的反馈计数。
func (r *Repo) GetMessageFeedbackCounts(
	ctx context.Context,
	messageIDs []uint,
) (map[uint]map[string]int64, error) {
	result := make(map[uint]map[string]int64, len(messageIDs))
	if len(messageIDs) == 0 {
		return result, nil
	}

	rows := make([]messageFeedbackAggregateRow, 0)
	if err := r.db.WithContext(ctx).
		Model(&models.ConversationMessageFeedback{}).
		Select("message_id, feedback, COUNT(*) AS count").
		Where("message_id IN ?", messageIDs).
		Group("message_id, feedback").
		Scan(&rows).Error; err != nil {
		return nil, translateError(err)
	}

	for _, row := range rows {
		bucket := result[row.MessageID]
		if bucket == nil {
			bucket = map[string]int64{
				"up":   0,
				"down": 0,
			}
		}
		bucket[row.Feedback] = row.Count
		result[row.MessageID] = bucket
	}
	return result, nil
}

// CreateAttachments 批量创建附件。
func (r *Repo) CreateAttachments(ctx context.Context, items []domainconversation.Attachment) error {
	if len(items) == 0 {
		return nil
	}
	entities := make([]models.Attachment, 0, len(items))
	for i := range items {
		entities = append(entities, toAttachmentModel(&items[i]))
	}
	return translateError(r.db.WithContext(ctx).Create(&entities).Error)
}

const (
	chatRunEventScopeTraceBlock = "trace_block"
	chatRunEventScopeTraceEvent = "trace_event"
	chatRunEventScopeToolCall   = "tool_call"
	chatContextRecordSnapshot   = "snapshot"
	chatContextRecordArtifact   = "artifact"
)

const maxConversationEventDetailPayloadBytes = 1024 * 1024

func conversationEventPayloadSizeExpression(db *gorm.DB) string {
	if db != nil && db.Dialector != nil {
		switch db.Dialector.Name() {
		case "postgres":
			return "OCTET_LENGTH(payload_json)"
		case "sqlite":
			return "LENGTH(CAST(payload_json AS BLOB))"
		}
	}
	return "LENGTH(payload_json)"
}

func conversationEventSummarySelectColumns(db *gorm.DB) []string {
	payloadSize := conversationEventPayloadSizeExpression(db)
	return []string{
		"id",
		"message_id",
		"conversation_id",
		"user_id",
		"run_id",
		"event_scope",
		"event_id",
		"event_type",
		"phase",
		"stage",
		"round_id",
		"parent_event_id",
		"status",
		"title",
		"summary",
		"seq",
		"tool_call_id",
		"tool_name",
		"latency_ms",
		"started_at",
		"ended_at",
		"created_at",
		"updated_at",
		payloadSize + " AS payload_size_bytes",
		fmt.Sprintf("CASE WHEN %s > %d THEN TRUE ELSE FALSE END AS payload_omitted", payloadSize, maxConversationEventDetailPayloadBytes),
	}
}

func conversationEventDetailSelectColumns(db *gorm.DB) []string {
	payloadSize := conversationEventPayloadSizeExpression(db)
	return append(
		conversationEventSummarySelectColumns(db),
		"content_markdown",
		fmt.Sprintf("CASE WHEN %s <= %d THEN payload_json ELSE '' END AS payload_json", payloadSize, maxConversationEventDetailPayloadBytes),
		"input_json",
		"output_json",
		"error_json",
	)
}

// CreateConversationRun 写入会话运行日志。
func (r *Repo) CreateConversationRun(ctx context.Context, item *domainconversation.Run) error {
	entity := toConversationRunModel(item)
	if err := r.db.WithContext(ctx).Create(&entity).Error; err != nil {
		return translateError(err)
	}
	*item = toConversationRunDomain(entity)
	return nil
}

// EnsureConversationRun inserts a run row when missing so mid-flight moderation updates have a target.
func (r *Repo) EnsureConversationRun(ctx context.Context, item *domainconversation.Run) error {
	if item == nil || strings.TrimSpace(item.RunID) == "" {
		return nil
	}
	entity := toConversationRunModel(item)
	return translateError(r.db.WithContext(ctx).
		Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "run_id"}},
			DoNothing: true,
		}).
		Create(&entity).Error)
}

// UpsertConversationRun writes the final run snapshot (create or full update by run_id).
func (r *Repo) UpsertConversationRun(ctx context.Context, item *domainconversation.Run) error {
	if item == nil || strings.TrimSpace(item.RunID) == "" {
		return nil
	}
	entity := toConversationRunModel(item)
	err := r.db.WithContext(ctx).
		Clauses(clause.OnConflict{
			Columns: []clause.Column{{Name: "run_id"}},
			DoUpdates: clause.AssignmentColumns([]string{
				"request_id",
				"user_id",
				"conversation_id",
				"task_type",
				"endpoint",
				"provider",
				"provider_protocol",
				"upstream_id",
				"upstream_model_id",
				"upstream_name",
				"requested_model_name",
				"platform_model_name",
				"routed_binding_code",
				"model_vendor",
				"model_icon",
				"upstream_model_name",
				"input_tokens",
				"output_tokens",
				"cache_read_tokens",
				"cache_write_tokens",
				"reasoning_tokens",
				"tool_calls_count",
				"first_token_latency_ms",
				"total_latency_ms",
				"status",
				"error_code",
				"error_message",
				"moderation_state",
				"moderation_event_id",
				"moderation_categories_json",
				"started_at",
				"ended_at",
				"updated_at",
			}),
		}).
		Create(&entity).Error
	if err != nil {
		return translateError(err)
	}
	*item = toConversationRunDomain(entity)
	return nil
}

// UpsertConversationMessageTrace 写入或更新消息轨迹。
func (r *Repo) UpsertConversationMessageTrace(ctx context.Context, item *domainconversation.MessageTrace) error {
	if item == nil {
		return nil
	}
	entity := toConversationMessageTraceModel(item)
	return translateError(r.db.WithContext(ctx).
		Clauses(clause.OnConflict{
			Columns: []clause.Column{
				{Name: "run_id"},
				{Name: "event_scope"},
				{Name: "event_id"},
			},
			DoUpdates: clause.AssignmentColumns([]string{
				"message_id",
				"conversation_id",
				"user_id",
				"event_type",
				"phase",
				"stage",
				"round_id",
				"parent_event_id",
				"status",
				"title",
				"summary",
				"content_markdown",
				"payload_json",
				"seq",
				"started_at",
				"ended_at",
				"updated_at",
			}),
		}).
		Create(&entity).Error)
}

// ListConversationMessageTracesByMessageIDs 查询消息轨迹。
func (r *Repo) ListConversationMessageTracesByMessageIDs(ctx context.Context, messageIDs []uint) ([]domainconversation.MessageTrace, error) {
	items := make([]models.ChatRunEvent, 0, len(messageIDs))
	if len(messageIDs) == 0 {
		return []domainconversation.MessageTrace{}, nil
	}
	if err := r.db.WithContext(ctx).
		Select(conversationEventDetailSelectColumns(r.db)).
		Where("message_id IN ? AND event_scope = ?", messageIDs, chatRunEventScopeTraceBlock).
		Order("message_id ASC, seq ASC, id ASC").
		Find(&items).Error; err != nil {
		return nil, translateError(err)
	}
	return toConversationMessageTraceDomains(items), nil
}

// UpsertConversationMessageTraceEvent 写入或更新消息轨迹事件。
func (r *Repo) UpsertConversationMessageTraceEvent(ctx context.Context, item *domainconversation.MessageTraceEventRow) error {
	if item == nil {
		return nil
	}
	entity := toConversationMessageTraceEventModel(item)
	return translateError(r.db.WithContext(ctx).
		Clauses(clause.OnConflict{
			Columns: []clause.Column{
				{Name: "run_id"},
				{Name: "event_scope"},
				{Name: "event_id"},
			},
			DoUpdates: clause.AssignmentColumns([]string{
				"message_id",
				"conversation_id",
				"user_id",
				"event_type",
				"phase",
				"stage",
				"round_id",
				"parent_event_id",
				"status",
				"title",
				"summary",
				"content_markdown",
				"payload_json",
				"seq",
				"started_at",
				"ended_at",
				"updated_at",
			}),
		}).
		Create(&entity).Error)
}

// ListConversationMessageTraceEventsByMessageIDs 查询消息轨迹事件。
func (r *Repo) ListConversationMessageTraceEventsByMessageIDs(ctx context.Context, messageIDs []uint) ([]domainconversation.MessageTraceEventRow, error) {
	items := make([]models.ChatRunEvent, 0, len(messageIDs))
	if len(messageIDs) == 0 {
		return []domainconversation.MessageTraceEventRow{}, nil
	}
	if err := r.db.WithContext(ctx).
		Select(conversationEventDetailSelectColumns(r.db)).
		Where("message_id IN ? AND event_scope = ?", messageIDs, chatRunEventScopeTraceEvent).
		Order("message_id ASC, seq ASC, id ASC").
		Find(&items).Error; err != nil {
		return nil, translateError(err)
	}
	return toConversationMessageTraceEventDomains(items), nil
}

// CreateConversationToolCalls 批量写入工具调用日志。
func (r *Repo) CreateConversationToolCall(ctx context.Context, item *domainconversation.ToolCall) error {
	if item == nil {
		return nil
	}
	entity := toConversationToolCallModel(item)
	if err := r.db.WithContext(ctx).Create(&entity).Error; err != nil {
		return translateError(err)
	}
	item.ID = entity.ID
	item.CreatedAt = entity.CreatedAt
	item.UpdatedAt = entity.UpdatedAt
	return nil
}

func (r *Repo) CreateConversationToolCalls(ctx context.Context, items []domainconversation.ToolCall) error {
	if len(items) == 0 {
		return nil
	}
	entities := make([]models.ChatRunEvent, 0, len(items))
	for i := range items {
		entities = append(entities, toConversationToolCallModel(&items[i]))
	}
	if err := r.db.WithContext(ctx).Create(&entities).Error; err != nil {
		return translateError(err)
	}
	for index := range items {
		if index < len(entities) {
			items[index].ID = entities[index].ID
			items[index].CreatedAt = entities[index].CreatedAt
			items[index].UpdatedAt = entities[index].UpdatedAt
		}
	}
	return nil
}

// ListConversationRuns 分页查询会话运行日志。
func (r *Repo) ListConversationRuns(
	ctx context.Context,
	userID uint,
	conversationID uint,
	offset int,
	limit int,
) ([]domainconversation.Run, int64, error) {
	items := make([]models.ConversationRun, 0)
	var total int64

	query := r.db.WithContext(ctx).
		Model(&models.ConversationRun{}).
		Where("user_id = ? AND conversation_id = ?", userID, conversationID)
	if err := query.Count(&total).Error; err != nil {
		return nil, 0, translateError(err)
	}
	if err := query.
		Order("id DESC").
		Offset(offset).
		Limit(limit).
		Find(&items).Error; err != nil {
		return nil, 0, translateError(err)
	}
	return toConversationRunDomains(items), total, nil
}

// ListConversationEventLogs 分页查询管理员对话事件日志。
func (r *Repo) ListConversationEventLogs(
	ctx context.Context,
	filter repository.ConversationEventLogListFilter,
	offset int,
	limit int,
) ([]domainconversation.EventLog, int64, error) {
	items := make([]models.ChatRunEvent, 0)
	var total int64
	query := r.db.WithContext(ctx).Model(&models.ChatRunEvent{})
	if filter.UserID > 0 {
		query = query.Where("user_id = ?", filter.UserID)
	}
	if filter.ConversationID > 0 {
		query = query.Where("conversation_id = ?", filter.ConversationID)
	}
	if search := strings.TrimSpace(filter.Query); search != "" {
		like := "%" + strings.ToLower(search) + "%"
		query = query.Where(
			"LOWER(run_id) LIKE ? OR LOWER(event_id) LIKE ? OR LOWER(event_type) LIKE ? OR LOWER(phase) LIKE ? OR LOWER(stage) LIKE ? OR LOWER(title) LIKE ? OR LOWER(summary) LIKE ? OR LOWER(tool_name) LIKE ?",
			like,
			like,
			like,
			like,
			like,
			like,
			like,
			like,
		)
	}
	if eventScope := strings.TrimSpace(filter.EventScope); eventScope != "" {
		query = query.Where("event_scope = ?", eventScope)
	}
	if eventType := strings.TrimSpace(filter.EventType); eventType != "" {
		query = query.Where("event_type = ?", eventType)
	}
	if status := strings.TrimSpace(filter.Status); status != "" {
		query = query.Where("status = ?", status)
	}
	if filter.CreatedFrom != nil {
		query = query.Where("created_at >= ?", *filter.CreatedFrom)
	}
	if filter.CreatedTo != nil {
		query = query.Where("created_at <= ?", *filter.CreatedTo)
	}
	if err := query.Count(&total).Error; err != nil {
		return nil, 0, translateError(err)
	}
	order := "created_at DESC, id DESC"
	switch strings.TrimSpace(filter.Sort) {
	case "created_asc":
		order = "created_at ASC, id ASC"
	case "latency_desc":
		order = "latency_ms DESC, id DESC"
	case "seq_asc":
		order = "run_id ASC, seq ASC, id ASC"
	}
	if err := query.
		Select(conversationEventSummarySelectColumns(r.db)).
		Order(order).
		Offset(offset).
		Limit(limit).
		Find(&items).Error; err != nil {
		return nil, 0, translateError(err)
	}
	results := toConversationEventLogDomains(items)
	if err := r.hydrateConversationEventRunMetadata(ctx, results); err != nil {
		return nil, 0, err
	}
	return results, total, nil
}

// GetConversationEventLog 查询单条管理员对话事件日志详情。
func (r *Repo) GetConversationEventLog(ctx context.Context, eventID uint) (*domainconversation.EventLog, error) {
	var item models.ChatRunEvent
	if err := r.db.WithContext(ctx).
		Select(conversationEventDetailSelectColumns(r.db)).
		Where("id = ?", eventID).
		First(&item).Error; err != nil {
		return nil, translateError(err)
	}
	results := toConversationEventLogDomains([]models.ChatRunEvent{item})
	if err := r.hydrateConversationEventRunMetadata(ctx, results); err != nil {
		return nil, err
	}
	if len(results) == 0 {
		return nil, repository.ErrNotFound
	}
	return &results[0], nil
}

func (r *Repo) hydrateConversationEventRunMetadata(ctx context.Context, results []domainconversation.EventLog) error {
	runIDs := make([]string, 0, len(results))
	seenRunIDs := make(map[string]struct{}, len(results))
	for _, item := range results {
		runID := strings.TrimSpace(item.RunID)
		if runID == "" {
			continue
		}
		if _, exists := seenRunIDs[runID]; exists {
			continue
		}
		seenRunIDs[runID] = struct{}{}
		runIDs = append(runIDs, runID)
	}
	if len(runIDs) == 0 {
		return nil
	}

	runs := make([]models.ConversationRun, 0, len(runIDs))
	if err := r.db.WithContext(ctx).
		Select("run_id", "provider_protocol", "upstream_name", "platform_model_name", "routed_binding_code", "upstream_model_name").
		Where("run_id IN ?", runIDs).
		Find(&runs).Error; err != nil {
		return translateError(err)
	}
	runsByID := make(map[string]models.ConversationRun, len(runs))
	for _, run := range runs {
		runsByID[run.RunID] = run
	}
	for index := range results {
		run, exists := runsByID[results[index].RunID]
		if !exists {
			continue
		}
		results[index].ProviderProtocol = run.ProviderProtocol
		results[index].UpstreamName = run.UpstreamName
		results[index].PlatformModelName = run.PlatformModelName
		results[index].RoutedBindingCode = run.RoutedBindingCode
		results[index].UpstreamModelName = run.UpstreamModelName
	}
	return nil
}

// ListConversationRunsByRunIDs 按运行 ID 查询会话运行快照。
func (r *Repo) ListConversationRunsByRunIDs(
	ctx context.Context,
	userID uint,
	conversationID uint,
	runIDs []string,
) ([]domainconversation.Run, error) {
	if len(runIDs) == 0 {
		return []domainconversation.Run{}, nil
	}
	items := make([]models.ConversationRun, 0, len(runIDs))
	if err := r.db.WithContext(ctx).
		Where("user_id = ? AND conversation_id = ? AND run_id IN ?", userID, conversationID, runIDs).
		Order("id ASC").
		Find(&items).Error; err != nil {
		return nil, translateError(err)
	}
	return toConversationRunDomains(items), nil
}

// GetMessageByID 按内部 ID 查询消息。
func (r *Repo) GetMessageByID(ctx context.Context, conversationID uint, messageID uint) (*domainconversation.Message, error) {
	var item models.Message
	if err := r.db.WithContext(ctx).
		Where("id = ? AND conversation_id = ?", messageID, conversationID).
		First(&item).Error; err != nil {
		return nil, translateError(err)
	}
	single := []models.Message{item}
	if err := r.hydrateMessageRefs(ctx, single); err != nil {
		return nil, err
	}
	if err := r.hydrateMessageAttachments(ctx, single); err != nil {
		return nil, err
	}
	item = single[0]
	result := toMessageDomain(item)
	return &result, nil
}

// GetLatestMessage 查询会话最新一条消息。
func (r *Repo) GetLatestMessage(ctx context.Context, conversationID uint) (*domainconversation.Message, error) {
	var item models.Message
	if err := r.db.WithContext(ctx).
		Where("conversation_id = ?", conversationID).
		Order("id DESC").
		Limit(1).
		First(&item).Error; err != nil {
		return nil, translateError(err)
	}
	single := []models.Message{item}
	if err := r.hydrateMessageRefs(ctx, single); err != nil {
		return nil, err
	}
	if err := r.hydrateMessageAttachments(ctx, single); err != nil {
		return nil, err
	}
	item = single[0]
	result := toMessageDomain(item)
	return &result, nil
}

// ListMessageAncestors 从指定消息向上遍历 parent_message_id 链，返回祖先消息（按 id ASC 排列）。
// 使用 WITH RECURSIVE CTE 一次往返代替原来最多 40 次单行查询（N+1 反模式）。
func (r *Repo) ListMessageAncestors(ctx context.Context, conversationID uint, leafMessageID uint, maxDepth int) ([]domainconversation.Message, error) {
	if maxDepth <= 0 {
		maxDepth = 40
	}
	if maxDepth > maxAncestorQueryDepth {
		maxDepth = maxAncestorQueryDepth
	}

	// WITH RECURSIVE：从叶节点沿 parent_message_id 向上递归，_depth 用于限制深度。
	// 外层用 SELECT * 取全部列：GORM Scan 按列名映射并忽略未匹配的列，_depth 会被自然丢弃，
	// 因此无需手写列清单（手写清单曾漏掉 reasoning_content 导致推理回传失效）。
	// deleted_at IS NULL 保持软删除语义。
	// 递归项约束 m.conversation_id：parent_message_id 上没有外键，「父消息同会话」仅靠
	// 应用层保证，一旦被破坏，跨会话内容会进入 prompt 并被烤进压缩摘要反复重放。
	const cteSQL = `
WITH RECURSIVE ancestors AS (
    SELECT *, 1 AS _depth
    FROM chat_messages
    WHERE id = ? AND conversation_id = ? AND deleted_at IS NULL
    UNION ALL
    SELECT m.*, a._depth + 1
    FROM chat_messages m
    INNER JOIN ancestors a ON m.id = a.parent_message_id
    WHERE a.parent_message_id IS NOT NULL
      AND a._depth < ?
      AND m.conversation_id = ?
      AND m.deleted_at IS NULL
)
SELECT * FROM ancestors
ORDER BY id ASC`

	path := make([]models.Message, 0)
	if err := r.db.WithContext(ctx).Raw(cteSQL, leafMessageID, conversationID, maxDepth, conversationID).Scan(&path).Error; err != nil {
		return nil, translateError(err)
	}

	if err := r.hydrateMessageRefs(ctx, path); err != nil {
		return nil, err
	}
	if err := r.hydrateMessageAttachments(ctx, path); err != nil {
		return nil, err
	}
	return toMessageDomains(path), nil
}

// ListLatestBranchPreviewMessages 返回最新叶节点所在分支末尾的轻量消息。
func (r *Repo) ListLatestBranchPreviewMessages(
	ctx context.Context,
	conversationID uint,
	maxDepth int,
	limit int,
) ([]domainconversation.Message, error) {
	if maxDepth <= 0 {
		maxDepth = 100
	}
	if maxDepth > maxAncestorQueryDepth {
		maxDepth = maxAncestorQueryDepth
	}
	if limit <= 0 {
		limit = 10
	}
	if limit > maxMessageQueryLimit {
		limit = maxMessageQueryLimit
	}

	type previewMessageRow struct {
		ID           uint   `gorm:"column:id"`
		PublicID     string `gorm:"column:public_id"`
		Role         string `gorm:"column:role"`
		Content      string `gorm:"column:content"`
		ErrorMessage string `gorm:"column:error_message"`
	}
	rows := make([]previewMessageRow, 0)
	const previewSQL = `
WITH RECURSIVE ancestors AS (
    SELECT id, conversation_id, parent_message_id, public_id, role, content, error_message, 1 AS depth
    FROM chat_messages
    WHERE id = (
        SELECT id
        FROM chat_messages
        WHERE conversation_id = ? AND deleted_at IS NULL
        ORDER BY id DESC
        LIMIT 1
    )
      AND conversation_id = ?
      AND deleted_at IS NULL
    UNION ALL
    SELECT m.id, m.conversation_id, m.parent_message_id, m.public_id, m.role, m.content, m.error_message, a.depth + 1
    FROM chat_messages AS m
    INNER JOIN ancestors AS a ON m.id = a.parent_message_id
    WHERE a.parent_message_id IS NOT NULL
      AND a.depth < ?
      AND m.conversation_id = ?
      AND m.deleted_at IS NULL
), visible_messages AS (
    SELECT id, public_id, role, content, error_message, depth
    FROM ancestors
    WHERE role IN ('user', 'assistant')
    ORDER BY depth ASC
    LIMIT ?
)
SELECT id, public_id, role, content, error_message
FROM visible_messages
ORDER BY depth DESC`
	if err := r.db.WithContext(ctx).
		Raw(previewSQL, conversationID, conversationID, maxDepth, conversationID, limit).
		Scan(&rows).Error; err != nil {
		return nil, translateError(err)
	}

	items := make([]domainconversation.Message, 0, len(rows))
	for _, row := range rows {
		items = append(items, domainconversation.Message{
			ID:             row.ID,
			ConversationID: conversationID,
			PublicID:       row.PublicID,
			Role:           row.Role,
			Content:        row.Content,
			ErrorMessage:   row.ErrorMessage,
		})
	}
	return items, nil
}

// ListMessageAncestorsUntil 从指定消息向上遍历 parent_message_id 链，直到命中 stopMessageID 或达到深度上限。
func (r *Repo) ListMessageAncestorsUntil(ctx context.Context, conversationID uint, leafMessageID uint, stopMessageID uint, maxDepth int) ([]domainconversation.Message, bool, error) {
	if maxDepth <= 0 {
		maxDepth = 200
	}
	if maxDepth > maxAncestorQueryDepth {
		maxDepth = maxAncestorQueryDepth
	}
	if leafMessageID == 0 || stopMessageID == 0 {
		return nil, false, repository.ErrInvalidInput
	}

	const cteSQL = `
WITH RECURSIVE ancestors AS (
    SELECT *, 1 AS _depth
    FROM chat_messages
    WHERE id = ? AND conversation_id = ? AND deleted_at IS NULL
    UNION ALL
    SELECT m.*, a._depth + 1
    FROM chat_messages m
    INNER JOIN ancestors a ON m.id = a.parent_message_id
    WHERE a.parent_message_id IS NOT NULL
      AND a._depth < ?
      AND a.id <> ?
      AND m.conversation_id = ?
      AND m.deleted_at IS NULL
)
SELECT * FROM ancestors
ORDER BY id ASC`

	path := make([]models.Message, 0)
	if err := r.db.WithContext(ctx).Raw(cteSQL, leafMessageID, conversationID, maxDepth, stopMessageID, conversationID).Scan(&path).Error; err != nil {
		return nil, false, translateError(err)
	}

	found := false
	for _, item := range path {
		if item.ID == stopMessageID {
			found = true
			break
		}
	}
	if err := r.hydrateMessageRefs(ctx, path); err != nil {
		return nil, false, err
	}
	if err := r.hydrateMessageAttachments(ctx, path); err != nil {
		return nil, false, err
	}
	return toMessageDomains(path), found, nil
}

// ListRecentMessages 查询会话最近消息窗口（按时间升序返回）。
func (r *Repo) ListRecentMessages(ctx context.Context, conversationID uint, limit int) ([]domainconversation.Message, int64, error) {
	if limit <= 0 {
		limit = defaultMessageQueryLimit
	}
	if limit > maxMessageQueryLimit {
		limit = maxMessageQueryLimit
	}
	var total int64
	if err := r.db.WithContext(ctx).
		Model(&models.Message{}).
		Where("conversation_id = ?", conversationID).
		Count(&total).Error; err != nil {
		return nil, 0, translateError(err)
	}
	offset := int(total) - limit
	if offset < 0 {
		offset = 0
	}
	items := make([]models.Message, 0)
	if err := r.db.WithContext(ctx).
		Where("conversation_id = ?", conversationID).
		Order("id ASC").
		Offset(offset).
		Limit(limit).
		Find(&items).Error; err != nil {
		return nil, 0, translateError(err)
	}
	if err := r.hydrateMessageRefs(ctx, items); err != nil {
		return nil, 0, err
	}
	if err := r.hydrateMessageAttachments(ctx, items); err != nil {
		return nil, 0, err
	}
	return toMessageDomains(items), total, nil
}

// CreateContextSnapshot 写入上下文压缩快照。
func (r *Repo) CreateContextSnapshot(ctx context.Context, item *domainconversation.ContextSnapshot) error {
	entity := toContextSnapshotModel(item)
	if err := r.db.WithContext(ctx).Create(&entity).Error; err != nil {
		return translateError(err)
	}
	*item = toContextSnapshotDomain(entity)
	return nil
}

// GetContextSnapshotByRunID 按运行 ID 查询上下文压缩快照。
func (r *Repo) GetContextSnapshotByRunID(ctx context.Context, runID string) (*domainconversation.ContextSnapshot, error) {
	var item models.ChatContextRecord
	if err := r.db.WithContext(ctx).
		Where("record_type = ? AND run_id = ?", chatContextRecordSnapshot, runID).
		Order("id DESC").
		Limit(1).
		First(&item).Error; err != nil {
		return nil, translateError(err)
	}
	result := toContextSnapshotDomain(item)
	return &result, nil
}

// GetLatestContextSnapshot 查询最近一次上下文压缩快照。
func (r *Repo) GetLatestContextSnapshot(ctx context.Context, conversationID uint) (*domainconversation.ContextSnapshot, error) {
	var item models.ChatContextRecord
	if err := r.db.WithContext(ctx).
		Where("record_type = ? AND conversation_id = ?", chatContextRecordSnapshot, conversationID).
		Order("id DESC").
		Limit(1).
		First(&item).Error; err != nil {
		return nil, translateError(err)
	}
	result := toContextSnapshotDomain(item)
	return &result, nil
}

// GetFirstActiveUpstream 查询第一个激活的上游（用于 embedding API）。
func (r *Repo) GetFirstActiveUpstream(ctx context.Context) (*models.LLMUpstream, error) {
	var item models.LLMUpstream
	if err := r.db.WithContext(ctx).
		Where("status = ?", "active").
		Order("id ASC").
		Limit(1).
		First(&item).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, translateError(err)
	}
	return &item, nil
}

// GetUpstreamByID 按 ID 查询上游。
func (r *Repo) GetUpstreamByID(ctx context.Context, upstreamID uint) (*models.LLMUpstream, error) {
	var item models.LLMUpstream
	if err := r.db.WithContext(ctx).
		Where("id = ?", upstreamID).
		First(&item).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, translateError(err)
	}
	return &item, nil
}











// GetUserSettingValue 查询指定用户的单个配置值，不存在时返回 ""。
func (r *Repo) GetUserSettingValue(ctx context.Context, userID uint, key string) (string, error) {
	var item models.UserSetting
	err := r.db.WithContext(ctx).
		Where("user_id = ? AND key = ?", userID, key).
		First(&item).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return "", nil
		}
		return "", translateError(err)
	}
	return item.Value, nil
}

// GetUserSettingValues 批量查询指定用户的配置值，缺失的 key 返回空字符串。
func (r *Repo) GetUserSettingValues(ctx context.Context, userID uint, keys []string) (map[string]string, error) {
	values := make(map[string]string, len(keys))
	for _, key := range keys {
		values[key] = ""
	}
	if len(keys) == 0 {
		return values, nil
	}
	var items []models.UserSetting
	if err := r.db.WithContext(ctx).
		Where("user_id = ? AND key IN ?", userID, keys).
		Find(&items).Error; err != nil {
		return nil, translateError(err)
	}
	for _, item := range items {
		values[item.Key] = item.Value
	}
	return values, nil
}


func (r *Repo) hydrateMessageRefs(ctx context.Context, items []models.Message) error {
	if len(items) == 0 {
		return nil
	}

	publicIDs := make(map[uint]string, len(items))
	for i := range items {
		publicIDs[items[i].ID] = items[i].PublicID
	}

	missingIDs := make(map[uint]struct{})
	for i := range items {
		if items[i].ParentMessageID != nil {
			if _, ok := publicIDs[*items[i].ParentMessageID]; !ok {
				missingIDs[*items[i].ParentMessageID] = struct{}{}
			}
		}
		if items[i].SourceMessageID != nil {
			if _, ok := publicIDs[*items[i].SourceMessageID]; !ok {
				missingIDs[*items[i].SourceMessageID] = struct{}{}
			}
		}
	}

	if len(missingIDs) > 0 {
		ids := make([]uint, 0, len(missingIDs))
		for id := range missingIDs {
			ids = append(ids, id)
		}
		refs := make([]models.Message, 0, len(ids))
		if err := r.db.WithContext(ctx).
			Select("id", "public_id").
			Where("id IN ?", ids).
			Find(&refs).Error; err != nil {
			return translateError(err)
		}
		for i := range refs {
			publicIDs[refs[i].ID] = refs[i].PublicID
		}
	}

	for i := range items {
		if items[i].ParentMessageID != nil {
			items[i].ParentPublicID = publicIDs[*items[i].ParentMessageID]
		}
		if items[i].SourceMessageID != nil {
			items[i].SourcePublicID = publicIDs[*items[i].SourceMessageID]
		}
	}
	return nil
}

type messageAttachmentSnapshotRow struct {
	MessageID              uint   `gorm:"column:message_id"`
	FileID                 string `gorm:"column:file_id"`
	Kind                   string `gorm:"column:kind"`
	FileName               string `gorm:"column:file_name"`
	MimeType               string `gorm:"column:mime_type"`
	DetectedMIME           string `gorm:"column:detected_mime"`
	FileCategory           string `gorm:"column:file_category"`
	FileSize               int64  `gorm:"column:file_size"`
	ProcessingStatus       string `gorm:"column:processing_status"`
	ProcessingReady        bool   `gorm:"column:processing_ready"`
	ProcessingErrorCode    string `gorm:"column:processing_error_code"`
	ProcessingErrorMessage string `gorm:"column:processing_error_message"`
	MetaJSON               string `gorm:"column:meta_json"`
}

func attachmentDurationSecondsFromMetaJSON(raw string) int64 {
	var metadata struct {
		DurationSeconds int64 `json:"duration_seconds"`
	}
	if json.Unmarshal([]byte(strings.TrimSpace(raw)), &metadata) != nil || metadata.DurationSeconds <= 0 {
		return 0
	}
	return metadata.DurationSeconds
}

func (r *Repo) hydrateMessageAttachments(ctx context.Context, items []models.Message) error {
	if len(items) == 0 {
		return nil
	}
	messageIDs := make([]uint, 0, len(items))
	for i := range items {
		items[i].Attachments = "[]"
		if items[i].ID != 0 {
			messageIDs = append(messageIDs, items[i].ID)
		}
	}
	if len(messageIDs) == 0 {
		return nil
	}

	rows := make([]messageAttachmentSnapshotRow, 0)
	if err := r.db.WithContext(ctx).
		Table("chat_attachments AS a").
		Select(strings.Join([]string{
			"a.message_id",
			"a.file_id",
			"a.kind",
			"a.file_name",
			"a.mime_type",
			"a.file_size",
			"a.meta_json",
			"fo.detected_mime",
			"fo.file_category",
			"fo.processing_status",
			"fo.processing_ready",
			"fo.processing_error_code",
			"fo.processing_error_message",
		}, ", ")).
		Joins("LEFT JOIN file_objects fo ON fo.file_id = a.file_id AND fo.user_id = a.user_id").
		Where("a.message_id IN ? AND a.status <> ?", messageIDs, "deleted").
		Order("a.id ASC").
		Scan(&rows).Error; err != nil {
		return translateError(err)
	}

	grouped := make(map[uint][]map[string]interface{}, len(rows))
	for _, row := range rows {
		payload := map[string]interface{}{
			"file_id":                  row.FileID,
			"kind":                     row.Kind,
			"file_name":                row.FileName,
			"mime_type":                row.MimeType,
			"detected_mime":            row.DetectedMIME,
			"file_category":            row.FileCategory,
			"file_size":                row.FileSize,
			"processing_status":        row.ProcessingStatus,
			"processing_ready":         row.ProcessingReady,
			"processing_error_code":    row.ProcessingErrorCode,
			"processing_error_message": row.ProcessingErrorMessage,
		}
		if durationSeconds := attachmentDurationSecondsFromMetaJSON(row.MetaJSON); durationSeconds > 0 {
			payload["duration_seconds"] = durationSeconds
		}
		grouped[row.MessageID] = append(grouped[row.MessageID], payload)
	}
	for i := range items {
		payload := grouped[items[i].ID]
		if len(payload) == 0 {
			continue
		}
		raw, err := json.Marshal(payload)
		if err != nil {
			items[i].Attachments = "[]"
			continue
		}
		items[i].Attachments = string(raw)
	}
	return nil
}

func toConversationDomain(item models.Conversation) domainconversation.Conversation {
	labelsJSON := strings.TrimSpace(item.LabelsJSON)
	if labelsJSON == "" {
		labelsJSON = "[]"
	}
	return domainconversation.Conversation{
		ID:                    item.ID,
		UserID:                item.UserID,
		ProjectID:             item.ProjectID,
		PublicID:              item.PublicID,
		Title:                 item.Title,
		LabelsJSON:            labelsJSON,
		LabelsManuallyManaged: item.LabelsManuallyManaged,
		Model:                 item.Model,
		Provider:              item.Provider,
		SessionKey:            item.SessionKey,
		IsStarred:             item.IsStarred,
		StarredAt:             item.StarredAt,
		MessageCount:          item.MessageCount,
		Status:                item.Status,
		ContextPolicy:         item.ContextPolicy,
		LastCompactedAt:       item.LastCompactedAt,
		LastResponseID:        item.LastResponseID,
		LastPromptFingerprint: item.LastPromptFingerprint,
		CreatedAt:             item.CreatedAt,
		UpdatedAt:             item.UpdatedAt,
	}
}

func toConversationDomains(items []models.Conversation) []domainconversation.Conversation {
	results := make([]domainconversation.Conversation, 0, len(items))
	for _, item := range items {
		results = append(results, toConversationDomain(item))
	}
	return results
}

func toConversationShareDomain(item models.ConversationShare) domainconversation.ConversationShare {
	return domainconversation.ConversationShare{
		ID:                    item.ID,
		ShareID:               item.ShareID,
		ConversationID:        item.ConversationID,
		UserID:                item.UserID,
		Status:                item.Status,
		TitleSnapshot:         item.TitleSnapshot,
		ModelSnapshot:         item.ModelSnapshot,
		MessageIDsJSON:        item.MessageIDsJSON,
		DefaultMessageIDsJSON: item.DefaultMessageIDsJSON,
		RevokedAt:             item.RevokedAt,
		RegeneratedAt:         item.RegeneratedAt,
		LastAccessedAt:        item.LastAccessedAt,
		CreatedAt:             item.CreatedAt,
		UpdatedAt:             item.UpdatedAt,
	}
}

func toConversationModel(item *domainconversation.Conversation) models.Conversation {
	if item == nil {
		return models.Conversation{}
	}
	labelsJSON := strings.TrimSpace(item.LabelsJSON)
	if labelsJSON == "" {
		labelsJSON = "[]"
	}
	return models.Conversation{
		UserID:                item.UserID,
		ProjectID:             item.ProjectID,
		PublicID:              item.PublicID,
		Title:                 item.Title,
		LabelsJSON:            labelsJSON,
		LabelsManuallyManaged: item.LabelsManuallyManaged,
		Model:                 item.Model,
		Provider:              item.Provider,
		SessionKey:            item.SessionKey,
		IsStarred:             item.IsStarred,
		StarredAt:             item.StarredAt,
		MessageCount:          item.MessageCount,
		Status:                item.Status,
		ContextPolicy:         item.ContextPolicy,
		LastCompactedAt:       item.LastCompactedAt,
		LastResponseID:        item.LastResponseID,
		LastPromptFingerprint: item.LastPromptFingerprint,
	}
}

func toConversationShareModel(item *domainconversation.ConversationShare) models.ConversationShare {
	if item == nil {
		return models.ConversationShare{}
	}
	return models.ConversationShare{
		ShareID:               item.ShareID,
		ConversationID:        item.ConversationID,
		UserID:                item.UserID,
		Status:                item.Status,
		TitleSnapshot:         item.TitleSnapshot,
		ModelSnapshot:         item.ModelSnapshot,
		MessageIDsJSON:        item.MessageIDsJSON,
		DefaultMessageIDsJSON: item.DefaultMessageIDsJSON,
		RevokedAt:             item.RevokedAt,
		RegeneratedAt:         item.RegeneratedAt,
		LastAccessedAt:        item.LastAccessedAt,
	}
}

func toUserDomain(item models.User) domainuser.User {
	return domainuser.User{
		ID:                    item.ID,
		PublicID:              item.PublicID,
		Username:              item.Username,
		DisplayName:           item.DisplayName,
		AvatarURL:             item.AvatarURL,
		Email:                 item.Email,
		Phone:                 item.Phone,
		Role:                  item.Role,
		Status:                item.Status,
		Timezone:              item.Timezone,
		Locale:                item.Locale,
		ProfilePreferences:    item.ProfilePreferences,
		AppearancePreferences: item.AppearancePreferences,
		EmailVerifiedAt:       item.EmailVerifiedAt,
		PhoneVerifiedAt:       item.PhoneVerifiedAt,
		LastLoginAt:           item.LastLoginAt,
		CreatedAt:             item.CreatedAt,
		UpdatedAt:             item.UpdatedAt,
	}
}

func toMessageDomain(item models.Message) domainconversation.Message {
	return domainconversation.Message{
		ID:                       item.ID,
		ConversationID:           item.ConversationID,
		UserID:                   item.UserID,
		PublicID:                 item.PublicID,
		ParentMessageID:          item.ParentMessageID,
		RunID:                    item.RunID,
		Role:                     item.Role,
		ContentType:              item.ContentType,
		Content:                  item.Content,
		ReasoningContent:         item.ReasoningContent,
		BranchReason:             item.BranchReason,
		SourceMessageID:          item.SourceMessageID,
		TokenUsage:               item.TokenUsage,
		InputTokens:              item.InputTokens,
		OutputTokens:             item.OutputTokens,
		CacheReadTokens:          item.CacheReadTokens,
		CacheWriteTokens:         item.CacheWriteTokens,
		ReasoningTokens:          item.ReasoningTokens,
		LatencyMS:                item.LatencyMS,
		BilledCurrency:           item.BilledCurrency,
		BilledNanousd:            item.BilledNanousd,
		PricingSnapshot:          item.PricingSnapshot,
		Status:                   item.Status,
		ErrorCode:                item.ErrorCode,
		ErrorMessage:             item.ErrorMessage,
		ModerationEventID:        item.ModerationEventID,
		ModerationCategoriesJSON: item.ModerationCategoriesJSON,
		KnowledgeSources:         parseMessageKnowledgeSources(item.KnowledgeSourcesJSON),
		Attachments:              item.Attachments,
		ParentPublicID:           item.ParentPublicID,
		SourcePublicID:           item.SourcePublicID,
		MyFeedback:               item.MyFeedback,
		ThumbsUpCount:            item.ThumbsUpCount,
		ThumbsDownCount:          item.ThumbsDownCount,
		EditedAt:                 item.EditedAt,
		CreatedAt:                item.CreatedAt,
		UpdatedAt:                item.UpdatedAt,
	}
}

func toMessageDomains(items []models.Message) []domainconversation.Message {
	results := make([]domainconversation.Message, 0, len(items))
	for _, item := range items {
		results = append(results, toMessageDomain(item))
	}
	return results
}

func toMessageModel(item *domainconversation.Message) models.Message {
	if item == nil {
		return models.Message{}
	}
	return models.Message{
		ConversationID:           item.ConversationID,
		UserID:                   item.UserID,
		PublicID:                 item.PublicID,
		ParentMessageID:          item.ParentMessageID,
		RunID:                    item.RunID,
		Role:                     item.Role,
		ContentType:              item.ContentType,
		Content:                  item.Content,
		ReasoningContent:         item.ReasoningContent,
		BranchReason:             item.BranchReason,
		SourceMessageID:          item.SourceMessageID,
		TokenUsage:               item.TokenUsage,
		InputTokens:              item.InputTokens,
		OutputTokens:             item.OutputTokens,
		CacheReadTokens:          item.CacheReadTokens,
		CacheWriteTokens:         item.CacheWriteTokens,
		ReasoningTokens:          item.ReasoningTokens,
		LatencyMS:                item.LatencyMS,
		BilledCurrency:           item.BilledCurrency,
		BilledNanousd:            item.BilledNanousd,
		PricingSnapshot:          item.PricingSnapshot,
		Status:                   item.Status,
		ErrorCode:                item.ErrorCode,
		ErrorMessage:             item.ErrorMessage,
		ModerationEventID:        item.ModerationEventID,
		ModerationCategoriesJSON: item.ModerationCategoriesJSON,
		KnowledgeSourcesJSON:     marshalMessageKnowledgeSources(item.KnowledgeSources),
		EditedAt:                 item.EditedAt,
	}
}

func toMessageFeedbackModel(item *domainconversation.MessageFeedback) models.ConversationMessageFeedback {
	if item == nil {
		return models.ConversationMessageFeedback{}
	}
	return models.ConversationMessageFeedback{
		UserID:         item.UserID,
		ConversationID: item.ConversationID,
		MessageID:      item.MessageID,
		Feedback:       item.Feedback,
	}
}

func toAttachmentModel(item *domainconversation.Attachment) models.Attachment {
	if item == nil {
		return models.Attachment{}
	}
	return models.Attachment{
		ConversationID: item.ConversationID,
		MessageID:      item.MessageID,
		UserID:         item.UserID,
		FileID:         item.FileID,
		Kind:           item.Kind,
		FileName:       item.FileName,
		MimeType:       item.MimeType,
		FileSize:       item.FileSize,
		SHA256:         item.SHA256,
		StoragePath:    item.StoragePath,
		Status:         item.Status,
		MetaJSON:       item.MetaJSON,
		UploadedAt:     item.UploadedAt,
	}
}

func toConversationRunDomain(item models.ConversationRun) domainconversation.Run {
	return domainconversation.Run{
		ID:                       item.ID,
		RunID:                    item.RunID,
		RequestID:                item.RequestID,
		UserID:                   item.UserID,
		ConversationID:           item.ConversationID,
		TaskType:                 item.TaskType,
		Endpoint:                 item.Endpoint,
		Provider:                 item.Provider,
		ProviderProtocol:         item.ProviderProtocol,
		UpstreamID:               item.UpstreamID,
		UpstreamModelID:          item.UpstreamModelID,
		UpstreamName:             item.UpstreamName,
		RequestedModelName:       item.RequestedModelName,
		PlatformModelName:        item.PlatformModelName,
		RoutedBindingCode:        item.RoutedBindingCode,
		ModelVendor:              item.ModelVendor,
		ModelIcon:                item.ModelIcon,
		UpstreamModelName:        item.UpstreamModelName,
		InputTokens:              item.InputTokens,
		OutputTokens:             item.OutputTokens,
		CacheReadTokens:          item.CacheReadTokens,
		CacheWriteTokens:         item.CacheWriteTokens,
		ReasoningTokens:          item.ReasoningTokens,
		ToolCallsCount:           item.ToolCallsCount,
		FirstTokenLatencyMS:      item.FirstTokenLatencyMS,
		TotalLatencyMS:           item.TotalLatencyMS,
		Status:                   item.Status,
		ErrorCode:                item.ErrorCode,
		ErrorMessage:             item.ErrorMessage,
		ModerationState:          item.ModerationState,
		ModerationEventID:        item.ModerationEventID,
		ModerationCategoriesJSON: item.ModerationCategoriesJSON,
		StartedAt:                item.StartedAt,
		EndedAt:                  item.EndedAt,
		CreatedAt:                item.CreatedAt,
		UpdatedAt:                item.UpdatedAt,
	}
}

func toConversationRunDomains(items []models.ConversationRun) []domainconversation.Run {
	results := make([]domainconversation.Run, 0, len(items))
	for _, item := range items {
		results = append(results, toConversationRunDomain(item))
	}
	return results
}

func toConversationEventLogDomains(items []models.ChatRunEvent) []domainconversation.EventLog {
	results := make([]domainconversation.EventLog, 0, len(items))
	for _, item := range items {
		results = append(results, domainconversation.EventLog{
			ID:               item.ID,
			MessageID:        item.MessageID,
			ConversationID:   item.ConversationID,
			UserID:           item.UserID,
			RunID:            item.RunID,
			EventScope:       item.EventScope,
			EventID:          item.EventID,
			EventType:        item.EventType,
			Phase:            item.Phase,
			Stage:            item.Stage,
			RoundID:          item.RoundID,
			ParentEventID:    item.ParentEventID,
			Status:           item.Status,
			Title:            item.Title,
			Summary:          item.Summary,
			ContentMarkdown:  item.ContentMarkdown,
			PayloadJSON:      item.PayloadJSON,
			PayloadSizeBytes: item.PayloadSizeBytes,
			PayloadOmitted:   item.PayloadOmitted,
			Seq:              item.Seq,
			ToolCallID:       item.ToolCallID,
			ToolName:         item.ToolName,
			LatencyMS:        item.LatencyMS,
			InputJSON:        item.InputJSON,
			OutputJSON:       item.OutputJSON,
			ErrorJSON:        item.ErrorJSON,
			StartedAt:        item.StartedAt,
			EndedAt:          item.EndedAt,
			CreatedAt:        item.CreatedAt,
			UpdatedAt:        item.UpdatedAt,
		})
	}
	return results
}

func toConversationRunModel(item *domainconversation.Run) models.ConversationRun {
	if item == nil {
		return models.ConversationRun{}
	}
	return models.ConversationRun{
		RunID:                    item.RunID,
		RequestID:                item.RequestID,
		UserID:                   item.UserID,
		ConversationID:           item.ConversationID,
		TaskType:                 item.TaskType,
		Endpoint:                 item.Endpoint,
		Provider:                 item.Provider,
		ProviderProtocol:         item.ProviderProtocol,
		UpstreamID:               item.UpstreamID,
		UpstreamModelID:          item.UpstreamModelID,
		UpstreamName:             item.UpstreamName,
		RequestedModelName:       item.RequestedModelName,
		PlatformModelName:        item.PlatformModelName,
		RoutedBindingCode:        item.RoutedBindingCode,
		ModelVendor:              item.ModelVendor,
		ModelIcon:                item.ModelIcon,
		UpstreamModelName:        item.UpstreamModelName,
		InputTokens:              item.InputTokens,
		OutputTokens:             item.OutputTokens,
		CacheReadTokens:          item.CacheReadTokens,
		CacheWriteTokens:         item.CacheWriteTokens,
		ReasoningTokens:          item.ReasoningTokens,
		ToolCallsCount:           item.ToolCallsCount,
		FirstTokenLatencyMS:      item.FirstTokenLatencyMS,
		TotalLatencyMS:           item.TotalLatencyMS,
		Status:                   item.Status,
		ErrorCode:                item.ErrorCode,
		ErrorMessage:             item.ErrorMessage,
		ModerationState:          defaultModerationState(item.ModerationState),
		ModerationEventID:        item.ModerationEventID,
		ModerationCategoriesJSON: defaultJSONArray(item.ModerationCategoriesJSON),
		StartedAt:                item.StartedAt,
		EndedAt:                  item.EndedAt,
	}
}

func defaultModerationState(value string) string {
	if strings.TrimSpace(value) == "" {
		return "not_required"
	}
	return value
}

func defaultJSONArray(value string) string {
	if strings.TrimSpace(value) == "" {
		return "[]"
	}
	return value
}

type messageKnowledgeSourceRecord struct {
	FileName   string  `json:"file_name"`
	FileID     string  `json:"file_id"`
	ChunkIndex int     `json:"chunk_index"`
	Score      float32 `json:"score"`
	Preview    string  `json:"preview"`
}

func marshalMessageKnowledgeSources(items []domainconversation.MessageKnowledgeSource) string {
	if len(items) == 0 {
		return "[]"
	}
	records := make([]messageKnowledgeSourceRecord, 0, len(items))
	for _, item := range items {
		records = append(records, messageKnowledgeSourceRecord{
			FileName: item.FileName, FileID: item.FileID, ChunkIndex: item.ChunkIndex,
			Score: item.Score, Preview: item.Preview,
		})
	}
	raw, err := json.Marshal(records)
	if err != nil {
		return "[]"
	}
	return string(raw)
}

func parseMessageKnowledgeSources(raw string) []domainconversation.MessageKnowledgeSource {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var records []messageKnowledgeSourceRecord
	if err := json.Unmarshal([]byte(raw), &records); err != nil {
		return nil
	}
	items := make([]domainconversation.MessageKnowledgeSource, 0, len(records))
	for _, record := range records {
		items = append(items, domainconversation.MessageKnowledgeSource{
			FileName: record.FileName, FileID: record.FileID, ChunkIndex: record.ChunkIndex,
			Score: record.Score, Preview: record.Preview,
		})
	}
	return items
}

func toConversationMessageTraceDomains(items []models.ChatRunEvent) []domainconversation.MessageTrace {
	results := make([]domainconversation.MessageTrace, 0, len(items))
	for _, item := range items {
		results = append(results, domainconversation.MessageTrace{
			ID:              item.ID,
			MessageID:       item.MessageID,
			ConversationID:  item.ConversationID,
			UserID:          item.UserID,
			RunID:           item.RunID,
			TraceType:       item.EventType,
			Status:          item.Status,
			Stage:           item.Stage,
			RoundID:         item.RoundID,
			ParentEventID:   item.ParentEventID,
			Title:           item.Title,
			Summary:         item.Summary,
			ContentMarkdown: item.ContentMarkdown,
			PayloadJSON:     item.PayloadJSON,
			Seq:             item.Seq,
			StartedAt:       item.StartedAt,
			EndedAt:         item.EndedAt,
			CreatedAt:       item.CreatedAt,
			UpdatedAt:       item.UpdatedAt,
		})
	}
	return results
}

func toConversationMessageTraceModel(item *domainconversation.MessageTrace) models.ChatRunEvent {
	if item == nil {
		return models.ChatRunEvent{}
	}
	startedAt := item.StartedAt
	if startedAt.IsZero() {
		startedAt = time.Now()
	}
	return models.ChatRunEvent{
		MessageID:       item.MessageID,
		ConversationID:  item.ConversationID,
		UserID:          item.UserID,
		RunID:           item.RunID,
		EventScope:      chatRunEventScopeTraceBlock,
		EventID:         fmt.Sprintf("message:%d:%s", item.MessageID, item.TraceType),
		EventType:       item.TraceType,
		Phase:           item.TraceType,
		Status:          item.Status,
		Stage:           item.Stage,
		RoundID:         item.RoundID,
		ParentEventID:   item.ParentEventID,
		Title:           item.Title,
		Summary:         item.Summary,
		ContentMarkdown: item.ContentMarkdown,
		PayloadJSON:     item.PayloadJSON,
		Seq:             item.Seq,
		StartedAt:       startedAt,
		EndedAt:         item.EndedAt,
	}
}

func toConversationMessageTraceEventDomains(items []models.ChatRunEvent) []domainconversation.MessageTraceEventRow {
	results := make([]domainconversation.MessageTraceEventRow, 0, len(items))
	for _, item := range items {
		results = append(results, domainconversation.MessageTraceEventRow{
			ID:              item.ID,
			MessageID:       item.MessageID,
			ConversationID:  item.ConversationID,
			UserID:          item.UserID,
			RunID:           item.RunID,
			EventID:         item.EventID,
			EventType:       item.EventType,
			Phase:           item.Phase,
			Stage:           item.Stage,
			RoundID:         item.RoundID,
			ParentEventID:   item.ParentEventID,
			Status:          item.Status,
			Title:           item.Title,
			Summary:         item.Summary,
			ContentMarkdown: item.ContentMarkdown,
			PayloadJSON:     item.PayloadJSON,
			Seq:             item.Seq,
			StartedAt:       item.StartedAt,
			EndedAt:         item.EndedAt,
			CreatedAt:       item.CreatedAt,
			UpdatedAt:       item.UpdatedAt,
		})
	}
	return results
}

func toConversationMessageTraceEventModel(item *domainconversation.MessageTraceEventRow) models.ChatRunEvent {
	if item == nil {
		return models.ChatRunEvent{}
	}
	startedAt := item.StartedAt
	if startedAt.IsZero() {
		startedAt = time.Now()
	}
	return models.ChatRunEvent{
		MessageID:       item.MessageID,
		ConversationID:  item.ConversationID,
		UserID:          item.UserID,
		RunID:           item.RunID,
		EventScope:      chatRunEventScopeTraceEvent,
		EventID:         item.EventID,
		EventType:       item.EventType,
		Phase:           item.Phase,
		Stage:           item.Stage,
		RoundID:         item.RoundID,
		ParentEventID:   item.ParentEventID,
		Status:          item.Status,
		Title:           item.Title,
		Summary:         item.Summary,
		ContentMarkdown: item.ContentMarkdown,
		PayloadJSON:     item.PayloadJSON,
		Seq:             item.Seq,
		StartedAt:       startedAt,
		EndedAt:         item.EndedAt,
	}
}

func toConversationToolCallModel(item *domainconversation.ToolCall) models.ChatRunEvent {
	if item == nil {
		return models.ChatRunEvent{}
	}
	startedAt := item.CreatedAt
	if startedAt.IsZero() {
		startedAt = time.Now()
	}
	eventID := strings.TrimSpace(item.ToolCallID)
	if eventID == "" {
		eventID = fmt.Sprintf("tool:%s:%d", strings.TrimSpace(item.ToolName), time.Now().UnixNano())
	}
	return models.ChatRunEvent{
		MessageID:      item.MessageID,
		ConversationID: item.ConversationID,
		UserID:         item.UserID,
		RunID:          item.RunID,
		EventScope:     chatRunEventScopeToolCall,
		EventID:        eventID,
		EventType:      item.ToolType,
		ToolCallID:     item.ToolCallID,
		ToolName:       item.ToolName,
		Status:         item.Status,
		LatencyMS:      item.LatencyMS,
		InputJSON:      item.InputJSON,
		OutputJSON:     item.OutputJSON,
		ErrorJSON:      item.ErrorJSON,
		StartedAt:      startedAt,
	}
}

func toContextSnapshotDomain(item models.ChatContextRecord) domainconversation.ContextSnapshot {
	return domainconversation.ContextSnapshot{
		ID:                    item.ID,
		ConversationID:        item.ConversationID,
		MessageID:             item.MessageID,
		UserID:                item.UserID,
		RunID:                 item.RunID,
		FromTurn:              item.FromTurn,
		ToTurn:                item.ToTurn,
		CoveredUntilMessageID: item.CoveredUntilMessageID,
		CoveredUntilPublicID:  item.CoveredUntilPublicID,
		CoveragePathHash:      item.CoveragePathHash,
		CoveredMessageCount:   item.CoveredMessageCount,
		SourceTokens:          item.SourceTokens,
		SummaryTokens:         item.SummaryTokens,
		SummaryText:           item.SummaryText,
		Strategy:              item.Strategy,
		CreatedAt:             item.CreatedAt,
		UpdatedAt:             item.UpdatedAt,
	}
}

func toContextSnapshotModel(item *domainconversation.ContextSnapshot) models.ChatContextRecord {
	if item == nil {
		return models.ChatContextRecord{}
	}
	return models.ChatContextRecord{
		RecordType:            chatContextRecordSnapshot,
		ConversationID:        item.ConversationID,
		MessageID:             item.MessageID,
		UserID:                item.UserID,
		RunID:                 item.RunID,
		FromTurn:              item.FromTurn,
		ToTurn:                item.ToTurn,
		CoveredUntilMessageID: item.CoveredUntilMessageID,
		CoveredUntilPublicID:  item.CoveredUntilPublicID,
		CoveragePathHash:      item.CoveragePathHash,
		CoveredMessageCount:   item.CoveredMessageCount,
		SourceTokens:          item.SourceTokens,
		SummaryTokens:         item.SummaryTokens,
		SummaryText:           item.SummaryText,
		Strategy:              item.Strategy,
	}
}
