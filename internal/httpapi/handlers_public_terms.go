package httpapi

import (
	"context"

	"github.com/nodal/controlplane/internal/gen/api"
	"github.com/nodal/controlplane/internal/terms"
)

// GetTerms serves the legal registry to anyone. It is the same registry a
// signed-in person accepts from, without any acceptance state: the public
// site renders these bytes so the text a visitor reads is the text whose hash
// an acceptance later records, and never an explainer written beside it.
func (s *Server) GetTerms(_ context.Context, _ api.GetTermsRequestObject) (api.GetTermsResponseObject, error) {
	docs, err := terms.Current()
	if err != nil {
		return nil, err
	}
	out := make([]api.PublicLegalDocument, 0, len(docs))
	for _, d := range docs {
		out = append(out, api.PublicLegalDocument{
			DocumentId:            string(d.ID),
			Version:               d.Version,
			Title:                 d.Title,
			ContentHash:           d.ContentHash,
			Requirement:           string(d.Requirement),
			CounselReviewRequired: d.CounselReviewRequired,
			Body:                  d.Body,
		})
	}
	return api.GetTerms200JSONResponse(out), nil
}
