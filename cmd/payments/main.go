package main

import (
	"database/sql"
	"distributed-look/handler"
	"distributed-look/postgres"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/go-zookeeper/zk"
)

var (
	host     = os.Getenv("PG_HOST")
	port     = os.Getenv("PG_PORT")
	user     = os.Getenv("PG_USER")
	password = os.Getenv("PG_PASSWORD")
	dbname   = os.Getenv("PG_DB")

	zkHost = os.Getenv("ZK_HOST")
	zkPort = os.Getenv("ZK_PORT") // 2181
)

var db *sql.DB

func main() {
	c, _, err := zk.Connect([]string{fmt.Sprintf("%s:%s", zkHost, zkPort)}, time.Second)
	if err != nil {
		panic(err)
	}
	defer c.Close()

	fmt.Println("Postgres Credentials", host, port, user, password, dbname)

	storage, err := postgres.New(host, port, user, password, dbname)
	if err != nil {
		panic(err)
	}
	defer storage.Close()

	h := handler.New(c, storage)

	router := gin.Default()
	db = storage.DB

	router.POST("/purchase/:transaction_id", h.Purchase)
	router.POST("/capture/:transaction_id", h.Capture)
	router.POST("/refund/:transaction_id", h.Refund)
	router.GET("/invoices/:transaction_id", h.GetInvoice)

	router.POST("/api/v2/capture/:transaction_id", h.CaptureV2)
	router.POST("/api/v3/capture/:transaction_id", h.CaptureV3)

	router.Run(":8080")
}

func handleProcess(c *gin.Context) {
	idStr := c.Param("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}

	tx, err := db.Begin()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to start transaction"})
		return
	}
	defer tx.Rollback()

	// Acquire advisory lock (transaction-scoped)
	_, err = tx.Exec("SELECT pg_advisory_xact_lock($1)", id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to acquire lock"})
		return
	}

	rows, err := db.Query("SELECT amount FROM transactions WHERE transaction_id=$1 AND status=$2;", idStr, "captured")
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to acquire lock"})
		return
	}
	var totalAmount int
	for rows.Next() {
		var amount int
		if err := rows.Scan(&amount); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to read records"})
			return
		}
		totalAmount += amount
	}

	fmt.Println("currently total amount", totalAmount)

	// Critical section (simulate insert or update)
	_, err = tx.Exec("INSERT INTO transactions(status, amount, transaction_id) VALUES ($1, $2, $3);", "captured", 100, idStr)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to insert data"})
		return
	}

	// Commit transaction (automatically releases lock)
	if err := tx.Commit(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to commit transaction"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": fmt.Sprintf("Process %d handled with lock", id)})
}
