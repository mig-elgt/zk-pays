package handler

import (
	"distributed-look/postgres"
	"errors"
	"fmt"
	"hash/fnv"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/go-zookeeper/zk"

	"github.com/satori/uuid"
)

type handler struct {
	zookeeper *zk.Conn
	storage   postgres.StorageService
}

func New(zookeeper *zk.Conn, storage postgres.StorageService) *handler {
	return &handler{zookeeper, storage}
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

	c.JSON(http.StatusCreated, gin.H{"transaction_id": transactionID, "status": "authorized", "amount": payload.Amount})
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

	_, err := h.zookeeper.Create(transactionIdLock, []byte(reqID.String()), zk.FlagPersistent, worldACL)
	if err != nil {
		if err.Error() == ErrNodeAlreadyExists.Error() {
			lockNodePath, err = h.zookeeper.Create(fmt.Sprintf("%s/capture-", transactionIdLock), []byte(reqID.String()), zk.FlagEphemeralSequential, worldACL)
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
		lockNodePath, err = h.zookeeper.Create(fmt.Sprintf("%s/capture-", transactionIdLock), []byte(reqID.String()), zk.FlagEphemeralSequential, worldACL)
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
	if err := h.zookeeper.Delete(lockNodePath, 0); err != nil {
		fmt.Println("could not delete znode", err)
		c.JSON(http.StatusInternalServerError, err)
		return
	}

	c.JSON(http.StatusOK, "capture applied")
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

	_, err := h.zookeeper.Create(transactionIdLock, []byte(reqID.String()), zk.FlagPersistent, worldACL)
	if err != nil {
		if err.Error() == ErrNodeAlreadyExists.Error() {
			lockNodePath, err = h.zookeeper.Create(fmt.Sprintf("%s/refund-", transactionIdLock), []byte(reqID.String()), zk.FlagEphemeralSequential, worldACL)
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
		lockNodePath, err = h.zookeeper.Create(fmt.Sprintf("%s/refund-", transactionIdLock), []byte(reqID.String()), zk.FlagEphemeralSequential, worldACL)
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

	if err := h.zookeeper.Delete(lockNodePath, 0); err != nil {
		fmt.Println("could not delete znode", err)
		c.JSON(http.StatusInternalServerError, err)
		return
	}

	c.JSON(http.StatusOK, "refund applied")
}

func (h *handler) lock(ch chan interface{}, rootNode string, reqID uuid.UUID) {
	fmt.Println("handle with req ID: ", rootNode, reqID)

	children, _, err := h.zookeeper.Children(rootNode)
	if err != nil {
		fmt.Println("could not get children watcher", err)
		ch <- err
		return
	}

	sort.Strings(children)

	var prevNode string
	for _, child := range children {
		data, _, err := h.zookeeper.Get(fmt.Sprintf("%s/%s", rootNode, child))
		if err != nil {
			ch <- err
			return
		}
		if string(data) == reqID.String() {
			break
		}
		prevNode = child
	}

	if prevNode == "" {
		ch <- "continue with handle process"
		return
	}

	_, _, event, err := h.zookeeper.GetW(fmt.Sprintf("%s/%s", rootNode, prevNode))
	if err != nil {
		ch <- err
		return
	}

	for eventType := range event {
		if eventType.Type == zk.EventNodeDeleted {
			ch <- "continue with handle process"
			return
		}
	}
}

func (h *handler) CaptureV2(c *gin.Context) {
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

	lockID, err := strconv.Atoi(transactionID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, err)
		return
	}

	err = h.storage.AcquireLock(lockID, reqID.String())
	if err != nil {
		c.JSON(http.StatusInternalServerError, err)
		return
	}

	defer func() {
		if err := h.storage.ReleaseLock(lockID, reqID.String()); err != nil {
			fmt.Println("could not release lock", err)
			return
		}
		fmt.Printf("release lock: %v; reqID: %v\n", lockID, reqID)
	}()

	fmt.Println("handle psp capture operation", reqID)
	time.Sleep(2 * time.Second)

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

	var captureTotalAmount int
	for _, txn := range txns {
		captureTotalAmount += txn.Amount
	}

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

	c.JSON(http.StatusOK, "capture applied")
}

func (h *handler) CaptureV3(c *gin.Context) {
	transactionID := c.Param("transaction_id")
	var payload struct {
		Amount int `json:"amount"`
	}

	if err := c.ShouldBindJSON(&payload); err != nil {
		fmt.Println("could not bind json")
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	lockID := generatePositiveInt64LockID(transactionID)

	isFinalCapture, err := h.storage.CaptureWithLock(payload.Amount, transactionID, lockID)
	if err != nil {
		fmt.Println("could not capture with lock")
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": err.Error()})
		return
	}

	if isFinalCapture {
		fmt.Println("performing final capture thorugh transaction service (psp)")
		fmt.Println("waiting PSP response")
		if err := h.storage.CreateTransaction(payload.Amount, "capturing", transactionID); err != nil {
			fmt.Println("could not create transaction: ", err)
			c.JSON(http.StatusInternalServerError, err)
			return
		}
		if err := h.storage.UpdateInvoiceStatus("captured", transactionID); err != nil {
			fmt.Println("could not update invoice: ", err)
			c.JSON(http.StatusInternalServerError, err)
			return
		}
	}

	c.JSON(http.StatusOK, gin.H{"message": "capture applied"})
}

func generatePositiveInt64LockID(input string) int64 {
	hasher := fnv.New64a()
	hasher.Write([]byte(input))
	// Limit value to MaxInt64 to avoid negative numbers
	return int64(hasher.Sum64() & 0x7FFFFFFFFFFFFFFF)
}
