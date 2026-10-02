package api

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/kc1wfotfm4/qry-lineage-catalog/internal/store"
)

// NewRouter wires the public HTTP surface. Every entry keeps the error shape the service
// contract in README.md describes.
func NewRouter(st *store.Store) *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	router := gin.New()
	router.Use(gin.Recovery())

	router.GET("/healthz", func(c *gin.Context) {
		if err := st.Ping(); err != nil {
			writeError(c, http.StatusServiceUnavailable, "storage_unavailable", "database is not available")
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "ok", "database": "ok"})
	})

	registerAssets(router, st)

	router.NoRoute(func(c *gin.Context) {
		writeError(c, http.StatusNotFound, "route_not_found", "no route matches this path")
	})
	return router
}
