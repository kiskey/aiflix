package server

import (
	"log"
	"runtime/debug"
	"time"

	"github.com/gofiber/fiber/v2"
)

func recoveryMiddleware(c *fiber.Ctx) error {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[PANIC] %v\n%s", r, debug.Stack())
			c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
				"error": "Internal server error",
			})
		}
	}()
	return c.Next()
}

func corsMiddleware(c *fiber.Ctx) error {
	c.Set("Access-Control-Allow-Origin", "*")
	c.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS, HEAD")
	c.Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Requested-With")
	c.Set("Access-Control-Max-Age", "86400")
	c.Set("Access-Control-Allow-Credentials", "false")

	if c.Method() == "OPTIONS" {
		return c.SendStatus(fiber.StatusNoContent)
	}
	return c.Next()
}

func loggingMiddleware(c *fiber.Ctx) error {
	start := time.Now()
	err := c.Next()
	duration := time.Since(start)
	status := c.Response().StatusCode()
	path := c.Path()
	if path != "/api/health" {
		log.Printf("[%s] %s %s %d %s", c.Method(), path, c.IP(), status, duration)
	}
	return err
}
