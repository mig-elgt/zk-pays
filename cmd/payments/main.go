package main

import (
	"distributed-look/handler"
	"distributed-look/postgres"
	"fmt"
	"os"
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

	router.POST("/purchase/:transaction_id", h.Purchase)
	router.POST("/capture/:transaction_id", h.Capture)
	router.POST("/refund/:transaction_id", h.Refund)
	router.GET("/invoices/:transaction_id", h.GetInvoice)

	router.Run(":8080")
}
