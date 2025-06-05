package postgres

import (
	"fmt"

	_ "github.com/lib/pq"
	"github.com/pkg/errors"
	psql "gorm.io/driver/postgres"
	"gorm.io/gorm"
)

type Invoice struct {
	ID            uint   `gorm:"primaryKey"`
	TransactionID string `gorm:"column:transaction_id"`
	Status        string `gorm:"column:status"`
	Amount        int    `gorm:"column:amount"`
}

type Transaction struct {
	ID            uint   `gorm:"primaryKey"`
	TransactionID string `gorm:"column:transaction_id"`
	Status        string `gorm:"column:status"`
	Amount        int    `gorm:"column:amount"`
}

type StorageService interface {
	CreateInvoice(amount int, status, transactionID string) error
	CreateTransaction(amount int, status, transactionID string) error
	GetInvoice(transactionID string) (*Invoice, error)
	GetTransactions(transactionID, status string) ([]*Transaction, error)
	UpdateInvoiceStatus(status, transactionID string) error

	AcquireLock(lockID int, reqID string) error
	ReleaseLock(lockID int, reqID string) error

	CaptureWithLock(amount int, transactionID string, lockID int64) (bool, error)

	Close() error
}

type postgres struct {
	DB *gorm.DB
}

func New(host, port, user, password, dbName string) (*postgres, error) {
	// Connect Postgres
	connect := fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=%s sslmode=disable", host, port, user, password, dbName)
	db, err := gorm.Open(psql.Open(connect), &gorm.Config{})
	if err != nil {
		return nil, errors.Wrap(err, "failed Open")
	}
	if err := db.AutoMigrate(&Invoice{}, &Transaction{}); err != nil {
		return nil, err
	}
	return &postgres{db}, nil
}

func (p *postgres) CreateInvoice(amount int, status, transactionID string) error {
	tx := p.DB.Begin()
	if tx.Error != nil {
		return tx.Error
	}
	if err := tx.Create(&Invoice{Status: status, Amount: amount, TransactionID: transactionID}).Error; err != nil {
		tx.Rollback()
		return err
	}
	// Commit if all operations succeed
	if err := tx.Commit().Error; err != nil {
		return err
	}
	return nil
}

func (p *postgres) CreateTransaction(amount int, status, transactionID string) error {
	tx := p.DB.Begin()
	if tx.Error != nil {
		return tx.Error
	}
	if err := tx.Create(&Transaction{Status: status, Amount: amount, TransactionID: transactionID}).Error; err != nil {
		tx.Rollback()
		return err
	}
	// Commit if all operations succeed
	if err := tx.Commit().Error; err != nil {
		return err
	}
	return nil
}

func (p *postgres) GetInvoice(transactionID string) (*Invoice, error) {
	var invoice Invoice
	if err := p.DB.Where("transaction_id = ?", transactionID).First(&invoice).Error; err != nil {
		return nil, err
	}
	return &invoice, nil
}

func (p *postgres) GetTransactions(transactionID, status string) ([]*Transaction, error) {
	var transactions []*Transaction
	if err := p.DB.Where(&Transaction{TransactionID: transactionID, Status: status}).Find(&transactions).Error; err != nil {
		return nil, err
	}
	return transactions, nil
}

func (p *postgres) UpdateInvoiceStatus(status, transactionID string) error {
	tx := p.DB.Begin()
	if tx.Error != nil {
		return tx.Error
	}
	if err := tx.Model(&Invoice{}).
		Where("transaction_id = ?", transactionID).
		Update("status", status).Error; err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit().Error
}

func (p *postgres) Close() error {
	sqlDB, err := p.DB.DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
}

func (p *postgres) AcquireLock(lockID int, reqID string) error {
	panic("not impl")
	// var lockObtained bool
	// for {
	// 	err := p.DB.QueryRow(fmt.Sprintf(`SELECT pg_try_advisory_lock(%d)`, lockID)).
	// 		Scan(&lockObtained)
	// 	if err != nil {
	// 		return fmt.Errorf("could not acquire lock: %v", err)
	// 	}
	// 	if lockObtained {
	// 		fmt.Printf("i got the lock: %v; reqID: %v\n", lockID, reqID)
	// 		break
	// 	}
	// 	fmt.Printf("waiting to acquire lock: %v; reqID: %v\n", lockID, reqID)
	// 	time.Sleep(time.Second * 2)
	// }
	// return nil
}

func (p *postgres) ReleaseLock(lockID int, reqID string) error {
	panic("not impl")
	// _, err := p.DB.Exec(fmt.Sprintf("SELECT pg_advisory_unlock(%d)", lockID))
	// if err != nil {
	// 	return fmt.Errorf("could no release lock: %v; %v", lockID, err)
	// }
	// return err
}

func (p *postgres) CaptureWithLock(amount int, transactionID string, lockID int64) (bool, error) {
	tx := p.DB.Begin()
	if tx.Error != nil {
		return false, tx.Error
	}

	defer func() {
		if r := recover(); r != nil {
			tx.Rollback()
			panic(r) // re-throw panic after rollback
		} else if tx.Error != nil {
			tx.Rollback()
		}
	}()

	fmt.Println("performing new capture: ", transactionID, lockID)

	// Acquire advisory lock (transaction-scoped)
	var dummy string
	if err := tx.Raw("SELECT pg_advisory_xact_lock(?)", lockID).Scan(&dummy).Error; err != nil {
		return false, err
	}

	// Critical section (simulate insert or update)
	var invoice Invoice
	if err := tx.Where("transaction_id = ?", transactionID).First(&invoice).Error; err != nil {
		return false, err
	}

	// Get invoice transactions
	var transactions []*Transaction
	if err := tx.Where("transaction_id = ?", transactionID).Where("status = ?", "capturing").Find(&transactions).Error; err != nil {
		return false, err
	}

	// Calculate total captured amount
	var balanceAmount, transactionCount int
	for _, trx := range transactions {
		balanceAmount += trx.Amount
		transactionCount++
	}

	fmt.Println("balance amount", balanceAmount, "transaction count", transactionCount)

	totalRequestedAmount := balanceAmount + amount
	if totalRequestedAmount > invoice.Amount {
		if err := tx.Commit().Error; err != nil {
			return false, err
		}
		return false, fmt.Errorf("capture_would_exceed_invoice_amount")
	}

	isFinalCapture := totalRequestedAmount == invoice.Amount

	if isFinalCapture {
		return true, tx.Commit().Error
	}

	// Create a new capture transaction
	if err := tx.Create(&Transaction{Status: "capturing", Amount: amount, TransactionID: transactionID}).Error; err != nil {
		return false, err
	}

	if invoice.Status != "capturing" {
		if err := tx.Model(&Invoice{}).Where("transaction_id = ?", transactionID).Update("status", "capturing").Error; err != nil {
			return false, err
		}
	}

	return false, tx.Commit().Error

}
