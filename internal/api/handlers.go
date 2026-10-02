package api

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/kc1wfotfm4/qry-lineage-catalog/internal/store"
)

func listAssets(st *store.Store) gin.HandlerFunc {
	return func(c *gin.Context) {
		var (
			assets []*store.Asset
			err    error
		)
		if assetType, ok := c.GetQuery("type"); ok {
			assets, err = st.ListAssetsByType(c.Request.Context(), assetType)
		} else {
			assets, err = st.ListAssets(c.Request.Context())
		}
		if err != nil {
			respondStorageError(c)
			return
		}
		c.JSON(http.StatusOK, assets)
	}
}

func listUpstream(st *store.Store) gin.HandlerFunc {
	return func(c *gin.Context) {
		assetID := c.Param("id")

		maxDepth := 0
		if rawDepth, ok := c.GetQuery("depth"); ok {
			depth, err := strconv.Atoi(rawDepth)
			if err != nil || depth < 1 || depth > 10 {
				respondError(c, http.StatusBadRequest, "invalid_depth", "depth must be an integer between 1 and 10")
				return
			}
			maxDepth = depth
		}

		target, upstream, err := st.Upstream(c.Request.Context(), assetID, maxDepth)
		if err != nil {
			respondStoreError(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"id":       target.ID,
			"upstream": upstream,
		})
	}
}

func respondStoreError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, store.ErrInvalidAsset):
		respondError(c, http.StatusBadRequest, "invalid_asset", "dependency ids must be non-empty and reference registered assets")
	case errors.Is(err, store.ErrLineageCycle):
		respondError(c, http.StatusConflict, "lineage_cycle", "the change would create a dependency cycle")
	case errors.Is(err, store.ErrAssetNotFound):
		respondError(c, http.StatusNotFound, "asset_not_found", "the requested asset is not registered")
	default:
		respondStorageError(c)
	}
}

func respondStorageError(c *gin.Context) {
	respondError(c, http.StatusServiceUnavailable, "storage_unavailable", "database is not available")
}

func respondError(c *gin.Context, status int, code, message string) {
	c.JSON(status, gin.H{"error": gin.H{"code": code, "message": message}})
}
