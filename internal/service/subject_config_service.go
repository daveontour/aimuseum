package service

import (
	"context"
	"fmt"
	"time"

	"github.com/daveontour/aimuseum/internal/model"
	"github.com/daveontour/aimuseum/internal/repository"
)

// SubjectConfigService handles GET /api/subject-configuration.
type SubjectConfigService struct {
	repo     *repository.SubjectConfigRepo
	appInstr *repository.AppSystemInstructionsRepo
}

// NewSubjectConfigService creates a SubjectConfigService.
func NewSubjectConfigService(repo *repository.SubjectConfigRepo, appInstr *repository.AppSystemInstructionsRepo) *SubjectConfigService {
	return &SubjectConfigService{repo: repo, appInstr: appInstr}
}

// SubjectConfigUpdateParams mirrors the POST body fields for subject_configuration only.
type SubjectConfigUpdateParams struct {
	SubjectName     string
	Gender          *string
	FamilyName      *string
	OtherNames      *string
	EmailAddresses  *string
	PhoneNumbers    *string
	WhatsAppHandle  *string
	InstagramHandle *string
}

// CreateOrUpdate upserts the singleton subject configuration row.
func (s *SubjectConfigService) CreateOrUpdate(ctx context.Context, p SubjectConfigUpdateParams) (*model.SubjectConfigResponse, error) {
	cfg, err := s.repo.Upsert(ctx, repository.UpsertSubjectConfigParams{
		SubjectName:     p.SubjectName,
		Gender:          p.Gender,
		FamilyName:      p.FamilyName,
		OtherNames:      p.OtherNames,
		EmailAddresses:  p.EmailAddresses,
		PhoneNumbers:    p.PhoneNumbers,
		WhatsAppHandle:  p.WhatsAppHandle,
		InstagramHandle: p.InstagramHandle,
	})
	if err != nil {
		return nil, err
	}
	return s.mergedResponse(ctx, cfg)
}

// GetConfiguration returns the singleton subject configuration merged with universal system instructions.
func (s *SubjectConfigService) GetConfiguration(ctx context.Context) (*model.SubjectConfigResponse, error) {
	cfg, err := s.repo.GetFirst(ctx)
	if err != nil {
		return nil, err
	}
	if cfg == nil {
		return nil, nil
	}
	return s.mergedResponse(ctx, cfg)
}

func (s *SubjectConfigService) mergedResponse(ctx context.Context, cfg *model.SubjectConfig) (*model.SubjectConfigResponse, error) {
	resp := toSubjectConfigResponse(cfg)
	if s.appInstr == nil {
		return resp, nil
	}
	ins, err := s.appInstr.Get(ctx)
	if err != nil {
		return nil, err
	}
	if ins != nil {
		resp.SystemInstructions = ins.ChatInstructions
		resp.CoreSystemInstructions = ins.CoreInstructions
		resp.QuestionSystemInstructions = ins.QuestionInstructions
	}
	return resp, nil
}

// AppSystemInstructionsUpdate holds the three universal prompt bodies.
type AppSystemInstructionsUpdate struct {
	ChatInstructions     string
	CoreInstructions     string
	QuestionInstructions string
}

// UpdateAppSystemInstructions replaces the singleton instruction row.
func (s *SubjectConfigService) UpdateAppSystemInstructions(ctx context.Context, p AppSystemInstructionsUpdate) error {
	if s.appInstr == nil {
		return fmt.Errorf("app system instructions repository not configured")
	}
	return s.appInstr.Upsert(ctx, p.ChatInstructions, p.CoreInstructions, p.QuestionInstructions)
}

func toSubjectConfigResponse(cfg *model.SubjectConfig) *model.SubjectConfigResponse {
	return &model.SubjectConfigResponse{
		ID:                     cfg.ID,
		SubjectName:            cfg.SubjectName,
		Gender:                 cfg.Gender,
		FamilyName:             cfg.FamilyName,
		OtherNames:             cfg.OtherNames,
		EmailAddresses:         cfg.EmailAddresses,
		PhoneNumbers:           cfg.PhoneNumbers,
		WhatsAppHandle:         cfg.WhatsAppHandle,
		InstagramHandle:        cfg.InstagramHandle,
		WritingStyleAI:         cfg.WritingStyleAI,
		PsychologicalProfileAI: cfg.PsychologicalProfileAI,
		CreatedAt:              isoString(ptrTime(cfg.CreatedAt.Time)),
		UpdatedAt:              isoString(ptrTime(cfg.UpdatedAt.Time)),
	}
}

// ptrTime converts a value time.Time to *time.Time so isoString can accept it.
func ptrTime(t time.Time) *time.Time {
	return &t
}
