package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/kc1wfotfm4/qry-lineage-catalog/internal/store"
)

type assetPayload struct {
	ID         *string   `json:"id"`
	Type       *string   `json:"type"`
	Owner      *string   `json:"owner"`
	Upstream   *[]string `json:"upstream"`
	Downstream *[]string `json:"downstream"`
}

func registerAsset(st *store.Store) gin.HandlerFunc {
	return func(c *gin.Context) {
		var payload assetPayload
		decoder := json.NewDecoder(c.Request.Body)
		if err := decoder.Decode(&payload); err != nil {
			var typeError *json.UnmarshalTypeError
			if errors.As(err, &typeError) {
				respondError(c, http.StatusBadRequest, "invalid_asset", "asset fields have the wrong type")
				return
			}
			respondError(c, http.StatusBadRequest, "invalid_request", "request body is not valid JSON")
			return
		}
		var extra json.RawMessage
		if err := decoder.Decode(&extra); err != io.EOF {
			respondError(c, http.StatusBadRequest, "invalid_request", "request body must contain a single JSON object")
			return
		}
		if payload.ID == nil || payload.Type == nil || payload.Owner == nil || payload.Upstream == nil || payload.Downstream == nil {
			respondError(c, http.StatusBadRequest, "invalid_asset", "asset must include id, type, owner, upstream and downstream")
			return
		}
		if strings.TrimSpace(*payload.ID) == "" || strings.TrimSpace(*payload.Type) == "" || strings.TrimSpace(*payload.Owner) == "" {
			respondError(c, http.StatusBadRequest, "invalid_asset", "id, type and owner must be non-empty strings")
			return
		}
		for _, ref := range *payload.Upstream {
			if strings.TrimSpace(ref) == "" {
				respondError(c, http.StatusBadRequest, "invalid_asset", "dependency ids must be non-empty and reference registered assets")
				return
			}
		}
		for _, ref := range *payload.Downstream {
			if strings.TrimSpace(ref) == "" {
				respondError(c, http.StatusBadRequest, "invalid_asset", "dependency ids must be non-empty and reference registered assets")
				return
			}
		}

		asset := &store.Asset{
			ID:         *payload.ID,
			Type:       *payload.Type,
			Owner:      *payload.Owner,
			Upstream:   append([]string{}, *payload.Upstream...),
			Downstream: append([]string{}, *payload.Downstream...),
		}
		created, err := st.UpsertAsset(c.Request.Context(), asset)
		if err != nil {
			respondStoreError(c, err)
			return
		}
		status := http.StatusOK
		if created {
			status = http.StatusCreated
		}
		c.JSON(status, asset)
	}
}
