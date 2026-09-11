package service

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

var ErrIssueDocumentNotFound = errors.New("issue document not found")

var ErrIssueDocumentVersionConflict = errors.New("issue document version conflict; retry the submission")

type IssueDocumentService struct {
	Queries   *db.Queries
	TxStarter TxStarter
}

func NewIssueDocumentService(q *db.Queries, tx TxStarter) *IssueDocumentService {
	return &IssueDocumentService{Queries: q, TxStarter: tx}
}

type SubmitIssueDocumentParams struct {
	WorkspaceID      pgtype.UUID
	IssueID          pgtype.UUID
	Type             string
	Title            string
	Content          string
	ContentType      string
	FileAttachmentID pgtype.UUID
	Status           string
	AuthorType       string
	AuthorID         pgtype.UUID
}

func (s *IssueDocumentService) Submit(ctx context.Context, p SubmitIssueDocumentParams) (db.IssueDocument, error) {
	tx, err := s.TxStarter.Begin(ctx)
	if err != nil {
		return db.IssueDocument{}, err
	}
	defer tx.Rollback(ctx)
	qtx := s.Queries.WithTx(tx)

	nextVersion, err := qtx.GetIssueDocumentNextVersion(ctx, db.GetIssueDocumentNextVersionParams{
		WorkspaceID: p.WorkspaceID,
		IssueID:     p.IssueID,
		Type:        p.Type,
	})
	if err != nil {
		return db.IssueDocument{}, err
	}

	if err := qtx.SupersedeIssueDocumentByIssueType(ctx, db.SupersedeIssueDocumentByIssueTypeParams{
		WorkspaceID: p.WorkspaceID,
		IssueID:     p.IssueID,
		Type:        p.Type,
	}); err != nil {
		return db.IssueDocument{}, err
	}

	doc, err := qtx.CreateIssueDocument(ctx, db.CreateIssueDocumentParams{
		WorkspaceID:      p.WorkspaceID,
		IssueID:          p.IssueID,
		Type:             p.Type,
		Title:            p.Title,
		Content:          pgtype.Text{String: p.Content, Valid: p.Content != ""},
		ContentType:      p.ContentType,
		FileAttachmentID: p.FileAttachmentID,
		Version:          nextVersion,
		Status:           p.Status,
		AuthorType:       p.AuthorType,
		AuthorID:         p.AuthorID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return db.IssueDocument{}, ErrIssueDocumentNotFound
		}
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return db.IssueDocument{}, ErrIssueDocumentVersionConflict
		}
		return db.IssueDocument{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return db.IssueDocument{}, err
	}
	return doc, nil
}
