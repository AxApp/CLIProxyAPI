package gettokenshooks

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

type budgetWindowDefinitionLedger struct {
	Items []BudgetWindowDefinition `json:"items"`
}

var budgetWindowDefinitionPathState struct {
	sync.RWMutex
	path string
}

func SetBudgetWindowDefinitionPathFromConfig(configPath string) error {
	path, err := budgetWindowDefinitionPathFromConfig(configPath)
	if err != nil {
		return err
	}
	budgetWindowDefinitionPathState.Lock()
	budgetWindowDefinitionPathState.path = path
	budgetWindowDefinitionPathState.Unlock()
	return nil
}

func budgetWindowDefinitionPath() (string, error) {
	budgetWindowDefinitionPathState.RLock()
	path := budgetWindowDefinitionPathState.path
	budgetWindowDefinitionPathState.RUnlock()
	if strings.TrimSpace(path) != "" {
		return path, nil
	}
	return budgetWindowDefinitionPathFromConfig("")
}

func budgetWindowDefinitionPathFromConfig(configPath string) (string, error) {
	if dir := strings.TrimSpace(configPath); dir != "" {
		if strings.EqualFold(filepath.Base(dir), "config.yaml") || strings.EqualFold(filepath.Base(dir), "config.yml") {
			dir = filepath.Dir(dir)
		}
		return filepath.Join(dir, "budget-window-definitions", "config.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "gettokens-data", "budget-window-definitions", "config.json"), nil
}

func ConfigureBudgetWindowDefinitionRoutes(group *gin.RouterGroup, _ *handlers.BaseAPIHandler, _ *config.Config) {
	configureBudgetWindowDefinitionRoutes(group, defaultRateLimitStore)
}

func configureBudgetWindowDefinitionRoutes(group *gin.RouterGroup, usageStore *rateLimitStore) {
	if group == nil {
		return
	}
	group.GET("/gettokens/budget-window-definitions", func(c *gin.Context) {
		items, err := readBudgetWindowDefinitions()
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"items": items})
	})
	group.POST("/gettokens/budget-window-definitions", func(c *gin.Context) {
		var definition BudgetWindowDefinition
		if err := c.ShouldBindJSON(&definition); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if strings.TrimSpace(definition.ID) == "" {
			definition.ID = randomID("budget-window")
		}
		normalized, err := normalizeBudgetWindowDefinition(definition)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		items, err := readBudgetWindowDefinitions()
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		if budgetWindowDefinitionExists(items, normalized.ID) {
			c.JSON(http.StatusConflict, gin.H{"error": "budget window definition already exists", "id": normalized.ID})
			return
		}
		items = append(items, normalized)
		if err := writeBudgetWindowDefinitions(items); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"items": listBudgetWindowDefinitions(items)})
	})
	group.PUT("/gettokens/budget-window-definitions/:id", func(c *gin.Context) {
		id := strings.TrimSpace(c.Param("id"))
		var definition BudgetWindowDefinition
		if err := c.ShouldBindJSON(&definition); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if strings.TrimSpace(definition.ID) != "" && strings.TrimSpace(definition.ID) != id {
			c.JSON(http.StatusBadRequest, gin.H{"error": "budget window definition id is immutable"})
			return
		}
		definition.ID = id
		normalized, err := normalizeBudgetWindowDefinition(definition)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		items, err := readBudgetWindowDefinitions()
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		if !budgetWindowDefinitionExists(items, id) {
			c.JSON(http.StatusNotFound, gin.H{"error": "budget window definition not found"})
			return
		}
		items = upsertBudgetWindowDefinition(items, normalized)
		if err := writeBudgetWindowDefinitions(items); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"items": listBudgetWindowDefinitions(items)})
	})
	group.DELETE("/gettokens/budget-window-definitions/:id", func(c *gin.Context) {
		id := strings.TrimSpace(c.Param("id"))
		items, err := readBudgetWindowDefinitions()
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		updated, ok := disableBudgetWindowDefinition(items, id)
		if !ok {
			c.JSON(http.StatusNotFound, gin.H{"error": "budget window definition not found"})
			return
		}
		if err := writeBudgetWindowDefinitions(updated); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"items": listBudgetWindowDefinitions(updated)})
	})
	group.POST("/gettokens/budget-window-definitions/preview", func(c *gin.Context) {
		if usageStore == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "usage aggregator store is not initialized"})
			return
		}
		var request struct {
			AccountKey   string                         `json:"account_key"`
			AccountKey2  string                         `json:"accountKey"`
			Now          time.Time                      `json:"now,omitempty"`
			Definitions  []BudgetWindowDefinition       `json:"definitions,omitempty"`
			Calibrations []AccountQuotaUsageCalibration `json:"calibrations,omitempty"`
		}
		if err := c.ShouldBindJSON(&request); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		accountKey := strings.TrimSpace(request.AccountKey)
		if accountKey == "" {
			accountKey = strings.TrimSpace(request.AccountKey2)
		}
		if accountKey == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "account_key is required"})
			return
		}
		definitions := request.Definitions
		if len(definitions) == 0 {
			var err error
			definitions, err = readBudgetWindowDefinitions()
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
				return
			}
		}
		normalized := make([]BudgetWindowDefinition, 0, len(definitions))
		for _, definition := range definitions {
			next, err := normalizeBudgetWindowDefinition(definition)
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
				return
			}
			if next.Enabled {
				normalized = append(normalized, next)
			}
		}
		facts, err := BuildBudgetWindowFacts(c.Request.Context(), usageStore, accountKey, normalized, BudgetWindowFactsOptions{Now: request.Now, Calibrations: request.Calibrations})
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"items": facts})
	})
}

func readBudgetWindowDefinitions() ([]BudgetWindowDefinition, error) {
	path, err := budgetWindowDefinitionPath()
	if err != nil {
		return nil, err
	}
	body, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []BudgetWindowDefinition{}, nil
		}
		return nil, err
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return []BudgetWindowDefinition{}, nil
	}
	var ledger budgetWindowDefinitionLedger
	if err := json.Unmarshal(body, &ledger); err != nil {
		return nil, err
	}
	return listBudgetWindowDefinitions(ledger.Items), nil
}

func writeBudgetWindowDefinitions(items []BudgetWindowDefinition) error {
	path, err := budgetWindowDefinitionPath()
	if err != nil {
		return err
	}
	if err := ensureParentDir(path); err != nil {
		return err
	}
	ledger := budgetWindowDefinitionLedger{Items: listBudgetWindowDefinitions(items)}
	body, err := json.MarshalIndent(ledger, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(body, '\n'), 0o600)
}

func normalizeBudgetWindowDefinition(definition BudgetWindowDefinition) (BudgetWindowDefinition, error) {
	definition.ID = strings.TrimSpace(definition.ID)
	if definition.ID == "" {
		return BudgetWindowDefinition{}, fmt.Errorf("budget window definition id is required")
	}
	definition.Kind = normalizeBudgetWindowKind(definition.Kind)
	if definition.Kind == "" {
		return BudgetWindowDefinition{}, fmt.Errorf("budget window kind is required")
	}
	definition.Metric = normalizeBudgetWindowMetric(definition.Metric)
	switch definition.Metric {
	case BudgetWindowMetricTokens, BudgetWindowMetricRequests:
	default:
		return BudgetWindowDefinition{}, fmt.Errorf("unsupported budget window metric %q", definition.Metric)
	}
	if definition.Limit <= 0 {
		return BudgetWindowDefinition{}, fmt.Errorf("budget window limit must be positive")
	}
	switch definition.Kind {
	case BudgetWindowKindDaily:
		if _, _, err := budgetWindowLocation(definition.Timezone); err != nil {
			return BudgetWindowDefinition{}, err
		}
		definition.Semantics = ""
		definition.Days = 0
		definition.StartsAt = time.Time{}
		definition.EndsAt = time.Time{}
	case BudgetWindowKindMultiDay:
		if _, _, err := budgetWindowLocation(definition.Timezone); err != nil {
			return BudgetWindowDefinition{}, err
		}
		if definition.Days <= 0 {
			return BudgetWindowDefinition{}, fmt.Errorf("multi-day window requires positive days")
		}
		semantics := strings.ToLower(strings.TrimSpace(definition.Semantics))
		if semantics == "" {
			semantics = BudgetWindowSemanticsCalendar
		}
		if semantics != BudgetWindowSemanticsCalendar {
			return BudgetWindowDefinition{}, fmt.Errorf("unsupported multi-day semantics %q", definition.Semantics)
		}
		definition.Semantics = semantics
		definition.StartsAt = time.Time{}
		definition.EndsAt = time.Time{}
	case BudgetWindowKindBounded:
		if definition.StartsAt.IsZero() || definition.EndsAt.IsZero() {
			return BudgetWindowDefinition{}, fmt.Errorf("bounded window requires startsAt and endsAt")
		}
		if !definition.EndsAt.After(definition.StartsAt) {
			return BudgetWindowDefinition{}, fmt.Errorf("bounded window endsAt must be after startsAt")
		}
		definition.Timezone = strings.TrimSpace(definition.Timezone)
		definition.Semantics = ""
		definition.Days = 0
		definition.StartsAt = definition.StartsAt.UTC()
		definition.EndsAt = definition.EndsAt.UTC()
	default:
		return BudgetWindowDefinition{}, fmt.Errorf("unsupported budget window kind %q", definition.Kind)
	}
	return definition, nil
}

func listBudgetWindowDefinitions(items []BudgetWindowDefinition) []BudgetWindowDefinition {
	out := make([]BudgetWindowDefinition, 0, len(items))
	for _, item := range items {
		normalized, err := normalizeBudgetWindowDefinition(item)
		if err != nil {
			continue
		}
		out = append(out, normalized)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].ID < out[j].ID
	})
	return out
}

func upsertBudgetWindowDefinition(items []BudgetWindowDefinition, definition BudgetWindowDefinition) []BudgetWindowDefinition {
	out := make([]BudgetWindowDefinition, 0, len(items)+1)
	replaced := false
	for _, item := range items {
		if strings.TrimSpace(item.ID) == definition.ID {
			out = append(out, definition)
			replaced = true
			continue
		}
		out = append(out, item)
	}
	if !replaced {
		out = append(out, definition)
	}
	return listBudgetWindowDefinitions(out)
}

func disableBudgetWindowDefinition(items []BudgetWindowDefinition, id string) ([]BudgetWindowDefinition, bool) {
	id = strings.TrimSpace(id)
	out := make([]BudgetWindowDefinition, 0, len(items))
	found := false
	for _, item := range items {
		if strings.TrimSpace(item.ID) == id {
			item.Enabled = false
			found = true
		}
		out = append(out, item)
	}
	return listBudgetWindowDefinitions(out), found
}

func budgetWindowDefinitionExists(items []BudgetWindowDefinition, id string) bool {
	id = strings.TrimSpace(id)
	for _, item := range items {
		if strings.TrimSpace(item.ID) == id {
			return true
		}
	}
	return false
}
