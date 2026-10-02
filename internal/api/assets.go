package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/kc1wfotfm4/qry-lineage-catalog/internal/store"
)

type assetPayload struct {
	ID         string   `json:"id"`
	Type       string   `json:"type"`
	Owner      string   `json:"owner"`
	Upstream   []string `json:"upstream"`
	Downstream []string `json:"downstream"`
}

func registerAssets(router *gin.Engine, st *store.Store) {
	router.POST("/api/v1/assets", postAsset(st))
	router.GET("/api/v1/assets", listAssets(st))
	router.GET("/api/v1/assets/:id/upstream", upstreamAssets(st))
}

func postAsset(st *store.Store) gin.HandlerFunc {
	return func(c *gin.Context) {
		body, err := io.ReadAll(c.Request.Body)
		if err != nil {
			writeError(c, http.StatusBadRequest, "invalid_request", "the request body could not be read")
			return
		}

		var raw map[string]json.RawMessage
		if err := json.Unmarshal(body, &raw); err != nil || raw == nil {
			writeError(c, http.StatusBadRequest, "invalid_request", "the request body is not valid JSON")
			return
		}

		payload, ok := decodeAssetPayload(c, raw)
		if !ok {
			return
		}

		asset, created, err := st.PutAsset(store.Asset{
			ID:         payload.ID,
			Type:       payload.Type,
			Owner:      payload.Owner,
			Upstream:   payload.Upstream,
			Downstream: payload.Downstream,
		})
		switch {
		case err == nil:
			status := http.StatusOK
			if created {
				status = http.StatusCreated
			}
			c.JSON(status, asset)
		case errors.Is(err, store.ErrInvalidAsset):
			writeError(c, http.StatusBadRequest, "invalid_asset",
				"an asset field is missing or malformed, or a dependency is unknown")
		case errors.Is(err, store.ErrLineageCycle):
			writeError(c, http.StatusConflict, "lineage_cycle",
				"registering this asset would create a dependency cycle")
		default:
			writeError(c, http.StatusServiceUnavailable, "storage_unavailable",
				"database is not available")
		}
	}
}

func decodeAssetPayload(c *gin.Context, raw map[string]json.RawMessage) (assetPayload, bool) {
	id, ok := requiredString(raw, "id")
	if !ok {
		writeError(c, http.StatusBadRequest, "invalid_asset", "asset id must be a non-empty string")
		return assetPayload{}, false
	}
	assetType, ok := requiredString(raw, "type")
	if !ok {
		writeError(c, http.StatusBadRequest, "invalid_asset", "asset type must be a non-empty string")
		return assetPayload{}, false
	}
	owner, ok := requiredString(raw, "owner")
	if !ok {
		writeError(c, http.StatusBadRequest, "invalid_asset", "asset owner must be a non-empty string")
		return assetPayload{}, false
	}
	upstream, ok := requiredStringList(raw, "upstream")
	if !ok {
		writeError(c, http.StatusBadRequest, "invalid_asset", "upstream must be an array of non-empty strings")
		return assetPayload{}, false
	}
	downstream, ok := requiredStringList(raw, "downstream")
	if !ok {
		writeError(c, http.StatusBadRequest, "invalid_asset", "downstream must be an array of non-empty strings")
		return assetPayload{}, false
	}
	return assetPayload{
		ID:         id,
		Type:       assetType,
		Owner:      owner,
		Upstream:   upstream,
		Downstream: downstream,
	}, true
}

func requiredString(raw map[string]json.RawMessage, key string) (string, bool) {
	value, present := raw[key]
	if !present {
		return "", false
	}
	var text string
	if err := json.Unmarshal(value, &text); err != nil || text == "" {
		return "", false
	}
	return text, true
}

func requiredStringList(raw map[string]json.RawMessage, key string) ([]string, bool) {
	value, present := raw[key]
	if !present {
		return nil, false
	}
	var values []string
	if err := json.Unmarshal(value, &values); err != nil {
		return nil, false
	}
	if values == nil {
		return nil, false
	}
	for _, entry := range values {
		if entry == "" {
			return nil, false
		}
	}
	return values, true
}

func listAssets(st *store.Store) gin.HandlerFunc {
	return func(c *gin.Context) {
		assets, err := st.ListAssets(c.Query("type"))
		if err != nil {
			writeError(c, http.StatusServiceUnavailable, "storage_unavailable",
				"database is not available")
			return
		}
		c.JSON(http.StatusOK, assets)
	}
}

func upstreamAssets(st *store.Store) gin.HandlerFunc {
	return func(c *gin.Context) {
		targetID := c.Param("id")
		depth := 0
		if raw := c.Query("depth"); raw != "" {
			parsed, err := strconv.Atoi(raw)
			if err != nil || parsed < 1 || parsed > 10 {
				writeError(c, http.StatusBadRequest, "invalid_depth",
					"depth must be an integer between 1 and 10")
				return
			}
			depth = parsed
		}

		upstream, err := st.Upstream(targetID, depth)
		switch {
		case err == nil:
			c.JSON(http.StatusOK, gin.H{"id": targetID, "upstream": upstream})
		case errors.Is(err, store.ErrAssetNotFound):
			writeError(c, http.StatusNotFound, "asset_not_found", "the requested asset does not exist")
		default:
			writeError(c, http.StatusServiceUnavailable, "storage_unavailable",
				"database is not available")
		}
	}
}

func writeError(c *gin.Context, status int, code, message string) {
	c.JSON(status, gin.H{"error": gin.H{"code": code, "message": message}})
}
