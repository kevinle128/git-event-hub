package main

import (
	"net/http"
	"os"

	"github.com/joho/godotenv"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"go.uber.org/zap"

	"git-event-hub/internal/database"
	"git-event-hub/internal/handlers"
)

var logger *zap.Logger

func initLogger() {
	var err error
	logLevel := os.Getenv("LOG_LEVEL")
	if logLevel == "production" {
		logger, err = zap.NewProduction()
	} else {
		logger, err = zap.NewDevelopment()
	}
	if err != nil {
		panic(err)
	}
}

func main() {
	// Load environment variables
	godotenv.Load()

	// Initialize logger
	initLogger()
	defer logger.Sync()

	logger.Info("Starting git-event-hub server")

	// Initialize database
	db, err := database.InitDB()
	if err != nil {
		logger.Fatal("Failed to connect to database", zap.Error(err))
	}
	logger.Info("Database connected successfully")
	_ = db // Use db in your handlers

	// Get host from environment
	host := os.Getenv("HOST")
	if host == "" {
		host = "0.0.0.0"
	}
	// Get HTTP port from environment
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	addr := host + ":" + port

	logger.Info("Starting HTTP server", zap.String("address", addr))

	// CORS: pinned to CORS_ORIGIN when set, permissive otherwise.
	corsOrigin := os.Getenv("CORS_ORIGIN")
	if corsOrigin == "" {
		corsOrigin = "*"
	}

	// Create Echo instance
	e := echo.New()

	// Middleware
	e.Use(middleware.Logger())
	e.Use(middleware.Recover())
	e.Use(middleware.CORSWithConfig(middleware.CORSConfig{
		AllowOrigins: []string{corsOrigin},
	}))

	// Health check endpoint
	e.GET("/health", func(c echo.Context) error {
		return c.JSON(http.StatusOK, map[string]string{
			"status":  "ok",
			"message": "Server is running",
		})
	})

	// Root endpoint
	e.GET("/", func(c echo.Context) error {
		return c.JSON(http.StatusOK, map[string]string{
			"message": "Welcome to git-event-hub!",
		})
	})

	e.GET("/api/users", handlers.GetUsers)
	e.POST("/api/users", handlers.CreateUser)
	e.GET("/api/users/:id", handlers.GetUser)
	e.PUT("/api/users/:id", handlers.UpdateUser)
	e.DELETE("/api/users/:id", handlers.DeleteUser)
	e.GET("/api/posts", handlers.GetPosts)
	e.POST("/api/posts", handlers.CreatePost)
	e.GET("/api/posts/:id", handlers.GetPost)
	e.PUT("/api/posts/:id", handlers.UpdatePost)
	e.DELETE("/api/posts/:id", handlers.DeletePost)

	// Start server
	if err := e.Start(addr); err != nil && err != http.ErrServerClosed {
		logger.Fatal("Failed to start server", zap.Error(err))
	}
}
