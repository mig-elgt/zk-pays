package handler

import (
	"distributed-look/postgres"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/go-zookeeper/zk"

	"github.com/satori/uuid"
)

type handler struct {
	client  *zk.Conn
	storage postgres.StorageService
}

func New(client *zk.Conn, storage postgres.StorageService) *handler {
	return &handler{client, storage}
}

var ErrNodeAlreadyExists = errors.New("zk: node already exists")

type Invoice struct {
	Amount int
	Status string
}

type Transaction struct {
	TransactionID string
	Amount        int
	Status        string
	Type          string
}

func (h *handler) Purchase(c *gin.Context) {
	transactionID := c.Param("transaction_id")

	payload := Invoice{}
	if err := c.ShouldBindJSON(&payload); err != nil {
		fmt.Println("could not bind json")
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if err := h.storage.CreateInvoice(payload.Amount, "authorized", transactionID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusCreated, payload)
}

func (h *handler) GetInvoice(c *gin.Context) {
	transactionID := c.Param("transaction_id")
	invoice, err := h.storage.GetInvoice(transactionID)
	if err != nil {
		fmt.Println("could not get invoice", err)
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, invoice)
}

func (h *handler) Capture(c *gin.Context) {
	var payload struct {
		Amout int `json:"amount"`
	}

	if err := c.ShouldBindJSON(&payload); err != nil {
		fmt.Println("could not bind json")
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	reqID := uuid.NewV4()
	transactionID := c.Param("transaction_id")

	transactionIdLock := fmt.Sprintf("/lock-post-auth-%s", transactionID)
	worldACL := zk.WorldACL(zk.PermAll)

	var lockNodePath string

	_, err := h.client.Create(transactionIdLock, []byte(reqID.String()), zk.FlagPersistent, worldACL)
	if err != nil {
		if err.Error() == ErrNodeAlreadyExists.Error() {
			lockNodePath, err = h.client.Create(fmt.Sprintf("%s/capture-", transactionIdLock), []byte(reqID.String()), zk.FlagEphemeralSequential, worldACL)
			if err != nil {
				fmt.Println("could not create lock capture node", err)
				c.JSON(http.StatusInternalServerError, err)
				return
			}
		} else {
			c.JSON(http.StatusInternalServerError, err)
			return
		}
	} else {
		lockNodePath, err = h.client.Create(fmt.Sprintf("%s/capture-", transactionIdLock), []byte(reqID.String()), zk.FlagEphemeralSequential, worldACL)
		if err != nil {
			fmt.Println("could not create lock capture node", err)
			c.JSON(http.StatusInternalServerError, err)
			return
		}
	}

	watcher := func() <-chan interface{} {
		ch := make(chan interface{})
		go func() {
			h.lock(ch, transactionIdLock, reqID)
		}()
		return ch
	}()

	<-watcher

	fmt.Println("handle psp capture operation", reqID)
	time.Sleep(time.Second)

	if err := h.storage.CreateTransaction(payload.Amout, "captured", transactionID); err != nil {
		c.JSON(http.StatusInternalServerError, err)
		return
	}

	// Get Invoice by Transaction ID
	invoice, err := h.storage.GetInvoice(transactionID)
	if err != nil {
		fmt.Println("could not get invoice", err, transactionID)
		c.JSON(http.StatusInternalServerError, err)
		return
	}

	fmt.Println("got invoice", invoice)

	// Get Transactions by Transaction ID
	txns, err := h.storage.GetTransactions(transactionID, "captured")
	if err != nil {
		fmt.Println("could not get transactions by transaction_id", err, transactionID)
		c.JSON(http.StatusInternalServerError, err)
		return
	}

	fmt.Println("got transactions", txns)

	var captureTotalAmount int
	for _, txn := range txns {
		captureTotalAmount += txn.Amount
	}

	fmt.Println("transactions list", txns)
	fmt.Println("currently capture amount", captureTotalAmount)

	invoiceStatus := "captured"

	if invoice.Amount == captureTotalAmount {
		// Update invoice to captured
		if err := h.storage.UpdateInvoiceStatus(invoiceStatus, transactionID); err != nil {
			c.JSON(http.StatusInternalServerError, err)
			return
		}
	} else {
		// Update invoice to partial_captured
		invoiceStatus = "partial_capturing"
		if err := h.storage.UpdateInvoiceStatus(invoiceStatus, transactionID); err != nil {
			c.JSON(http.StatusInternalServerError, err)
			return
		}
	}

	// Release lock
	if err := h.client.Delete(lockNodePath, 0); err != nil {
		fmt.Println("could not delete znode", err)
		c.JSON(http.StatusInternalServerError, err)
		return
	}

	c.JSON(http.StatusOK, invoiceStatus)
}

func (h *handler) lock(ch chan interface{}, rootNode string, reqID uuid.UUID) {
	fmt.Println("handle with req ID: ", rootNode, reqID)
	children, _, err := h.client.Children(rootNode)
	if err != nil {
		fmt.Println("could not get children watcher", err)
		ch <- err
		return
	}

	sort.Strings(children)

	fmt.Println("got children", children)
	childrenNode := fmt.Sprintf("%s/%s", rootNode, children[0])
	data, _, event, err := h.client.GetW(childrenNode)
	if err != nil {
		ch <- err
		return
	}

	fmt.Println("znode data: ", string(data), reqID)
	if string(data) == reqID.String() {
		ch <- "continue with handle process"
		return
	}
	<-event
	h.lock(ch, rootNode, reqID)
}

func (h *handler) Refund(c *gin.Context) {
	var payload struct {
		Amout int `json:"amount"`
	}

	if err := c.ShouldBindJSON(&payload); err != nil {
		fmt.Println("could not bind json")
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	reqID := uuid.NewV4()
	transactionID := c.Param("transaction_id")

	transactionIdLock := fmt.Sprintf("/lock-post-auth-%s", transactionID)
	worldACL := zk.WorldACL(zk.PermAll)

	var lockNodePath string

	_, err := h.client.Create(transactionIdLock, []byte(reqID.String()), zk.FlagPersistent, worldACL)
	if err != nil {
		if err.Error() == ErrNodeAlreadyExists.Error() {
			lockNodePath, err = h.client.Create(fmt.Sprintf("%s/refund-", transactionIdLock), []byte(reqID.String()), zk.FlagEphemeralSequential, worldACL)
			if err != nil {
				fmt.Println("could not create lock capture node", err)
				c.JSON(http.StatusInternalServerError, err)
				return
			}
		} else {
			c.JSON(http.StatusInternalServerError, err)
			return
		}
	} else {
		lockNodePath, err = h.client.Create(fmt.Sprintf("%s/refund-", transactionIdLock), []byte(reqID.String()), zk.FlagEphemeralSequential, worldACL)
		if err != nil {
			fmt.Println("could not create lock capture node", err)
			c.JSON(http.StatusInternalServerError, err)
			return
		}
	}

	watcher := func() <-chan interface{} {
		ch := make(chan interface{})
		go func() {
			h.lock(ch, transactionIdLock, reqID)
		}()
		return ch
	}()

	<-watcher

	fmt.Println("handle request...", reqID)
	time.Sleep(2 * time.Second)

	if err := h.storage.CreateTransaction(payload.Amout, "refunded", transactionID); err != nil {
		c.JSON(http.StatusInternalServerError, err)
		return
	}

	// Get Invoice by Transaction ID
	invoice, err := h.storage.GetInvoice(transactionID)
	if err != nil {
		fmt.Println("could not get invoice", err, transactionID)
		c.JSON(http.StatusInternalServerError, err)
		return
	}

	fmt.Println("got invoice", invoice)

	// Get Transactions by Transaction ID
	txns, err := h.storage.GetTransactions(transactionID, "refunded")
	if err != nil {
		fmt.Println("could not get transactions by transaction_id", err, transactionID)
		c.JSON(http.StatusInternalServerError, err)
		return
	}

	fmt.Println("got transactions", txns)

	var refundTotalAmount int
	for _, txn := range txns {
		refundTotalAmount += txn.Amount
	}

	fmt.Println("transactions list", txns)
	fmt.Println("currently capture amount", refundTotalAmount)

	invoiceStatus := "refunded"

	if invoice.Amount == refundTotalAmount {
		// Update invoice to captured
		if err := h.storage.UpdateInvoiceStatus(invoiceStatus, transactionID); err != nil {
			c.JSON(http.StatusInternalServerError, err)
			return
		}
	} else {
		// Update invoice to partial_captured
		invoiceStatus = "partial_refunding"
		if err := h.storage.UpdateInvoiceStatus(invoiceStatus, transactionID); err != nil {
			c.JSON(http.StatusInternalServerError, err)
			return
		}
	}

	if err := h.client.Delete(lockNodePath, 0); err != nil {
		fmt.Println("could not delete znode", err)
		c.JSON(http.StatusInternalServerError, err)
		return
	}

	c.JSON(http.StatusOK, invoiceStatus)
}
