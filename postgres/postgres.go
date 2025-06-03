package postgres

import (
	"database/sql"
	"fmt"
	"time"

	_ "github.com/lib/pq"
	"github.com/pkg/errors"
)

type StorageService interface {
	CreateInvoice(amount int, status, transactionID string) error
	CreateTransaction(amount int, status, transactionID string) error
	GetInvoice(transactionID string) (*Invoice, error)
	GetTransactions(transactionID, status string) ([]*Transaction, error)
	UpdateInvoiceStatus(status, transactionID string) error

	AcquireLock(lockID int, reqID string) error
	ReleaseLock(lockID int, reqID string) error

	CaptureWithLock(amount int, transactionID int64) error

	Close() error
}

type postgres struct {
	DB *sql.DB
}

func New(host, port, user, password, dbName string) (*postgres, error) {
	// Connect Postgres
	connect := fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=%s sslmode=disable", host, port, user, password, dbName)
	db, err := sql.Open("postgres", connect)
	if err != nil {
		return nil, errors.Wrap(err, "failed Open")
	}

	// Ping Connection
	if err := db.Ping(); err != nil {
		return nil, errors.Wrap(err, "failed Ping")
	}

	// Create table if not exists
	strQuery := `
	CREATE TABLE IF NOT EXISTS invoices (
		id SERIAL PRIMARY KEY,
		transaction_id VARCHAR,
		status VARCHAR,
		amount INTEGER,
		created_at TIMESTAMP DEFAULT NOW()
	);
	CREATE TABLE IF NOT EXISTS transactions (
		id SERIAL PRIMARY KEY,
		transaction_id VARCHAR,
		status VARCHAR,
		amount INTEGER,
		created_at TIMESTAMP DEFAULT NOW()
	);
	`
	_, err = db.Exec(strQuery)
	if err != nil {
		return nil, err
	}
	return &postgres{db}, nil
}

func (p *postgres) CreateInvoice(amount int, status, transactionID string) error {
	query := `INSERT INTO invoices(status, amount, transaction_id) VALUES ($1, $2, $3);`
	if _, err := p.DB.Exec(query, status, amount, transactionID); err != nil {
		return errors.Wrap(err, "could not exec sql query")
	}
	return nil
}

func (p *postgres) CreateTransaction(amount int, status, transactionID string) error {
	query := `INSERT INTO transactions(status, amount, transaction_id) VALUES ($1, $2, $3);`
	if _, err := p.DB.Exec(query, status, amount, transactionID); err != nil {
		return errors.Wrap(err, "could not exec sql query")
	}
	return nil
}

type Transaction struct {
	Status string
	Amount int
	Kind   string
}

type Invoice struct {
	Status string
	Amount int
}

func (p *postgres) GetInvoice(transactionID string) (*Invoice, error) {
	var invoice Invoice
	if err := p.DB.QueryRow("SELECT status, amount FROM invoices WHERE transaction_id=$1;", transactionID).
		Scan(&invoice.Status, &invoice.Amount); err != nil {
		return nil, err
	}
	return &invoice, nil
}

func (p *postgres) GetTransactions(transactionID, status string) ([]*Transaction, error) {
	list := []*Transaction{}
	rows, err := p.DB.Query("SELECT status, amount FROM transactions WHERE transaction_id=$1 AND status=$2;", transactionID, status)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var txn Transaction
		if err := rows.Scan(&txn.Status, &txn.Amount); err != nil {
			return nil, err
		}
		list = append(list, &txn)
	}
	return list, nil
}

func (p *postgres) UpdateInvoiceStatus(status, transactionID string) error {
	query := ("UPDATE invoices SET status=$1 WHERE transaction_id=$2;")
	result, err := p.DB.Exec(query, status, transactionID)
	if err != nil {
		return errors.Wrapf(err, "failed to execute query")
	}
	total, err := result.RowsAffected()
	if err != nil {
		return errors.Wrap(err, "could not get rows affected")
	}
	if total == 0 {
		return fmt.Errorf("invoice not found")
	}
	return nil
}

func (p *postgres) Close() error {
	return p.DB.Close()
}

func (p *postgres) AcquireLock(lockID int, reqID string) error {
	var lockObtained bool
	for {
		err := p.DB.QueryRow(fmt.Sprintf(`SELECT pg_try_advisory_lock(%d)`, lockID)).
			Scan(&lockObtained)
		if err != nil {
			return fmt.Errorf("could not acquire lock: %v", err)
		}
		if lockObtained {
			fmt.Printf("i got the lock: %v; reqID: %v\n", lockID, reqID)
			break
		}
		fmt.Printf("waiting to acquire lock: %v; reqID: %v\n", lockID, reqID)
		time.Sleep(time.Second * 2)
	}
	return nil
}

func (p *postgres) ReleaseLock(lockID int, reqID string) error {
	_, err := p.DB.Exec(fmt.Sprintf("SELECT pg_advisory_unlock(%d)", lockID))
	if err != nil {
		return fmt.Errorf("could no release lock: %v; %v", lockID, err)
	}
	return err
}

func (p *postgres) CaptureWithLock(amount int, transactionID int64) error {
	tx, err := p.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Acquire advisory lock (transaction-scoped)
	_, err = tx.Exec("SELECT pg_advisory_xact_lock($1)", transactionID)
	if err != nil {
		return err
	}

	// Critical section (simulate insert or update)
	var invoice Invoice
	if err := p.DB.QueryRow("SELECT status, amount FROM invoices WHERE transaction_id=$1;", transactionID).
		Scan(&invoice.Status, &invoice.Amount); err != nil {
		return err
	}

	// Get invoice transactions
	rows, err := tx.Query("SELECT amount FROM transactions WHERE transaction_id=$1 AND status=$2;", transactionID, "capturing")
	if err != nil {
		return err
	}

	// Calculate total captured amount
	var balanceAmount, transactionCount int
	for rows.Next() {
		var amount int
		if err := rows.Scan(&amount); err != nil {
			return err
		}
		balanceAmount += amount
		transactionCount++
	}

	fmt.Println("balance amount", balanceAmount, "transaction count", transactionCount)

	if balanceAmount >= invoice.Amount {
		return fmt.Errorf("invoice already fully captured")
	}

	totalRequestedAmount := balanceAmount + amount
	if totalRequestedAmount > invoice.Amount {
		return fmt.Errorf("capture_would_exceed_invoice_amount")
	}

	isFinalCapture := totalRequestedAmount == invoice.Amount

	if !isFinalCapture {
		// Create a new capture transaction
		_, err = tx.Exec("INSERT INTO transactions(status, amount, transaction_id) VALUES ($1, $2, $3);", "capturing", amount, transactionID)
		if err != nil {
			return fmt.Errorf("could not create transaction: %v", err)
		}
	}

	// Only perform external capture when it's the final capture
	if isFinalCapture {
		fmt.Println("performing final capture thorugh transaction service (psp)")
		fmt.Println("waiting PSP response")
		// Create the final capture transaction
		_, err = tx.Exec("INSERT INTO transactions(status, amount, transaction_id) VALUES ($1, $2, $3);", "capturing", amount, transactionID)
		if err != nil {
			return fmt.Errorf("could not create transaction: %v", err)
		}
		// Update invoice status from capturing to captured
		_, err = tx.Exec("UPDATE invoices SET status=$1 WHERE transaction_id=$2;", "captured", transactionID)
		if err != nil {
			return fmt.Errorf("could not update invoice status: %v", err)
		}

		// Commit transaction (automatically releases lock)
		if err := tx.Commit(); err != nil {
			return err
		}

		return nil
	} else {
		// Update invoice status to capturing if not already
		if invoice.Status != "capturing" {
			_, err := tx.Exec("UPDATE invoices SET status=$1 WHERE transaction_id=$2;", "capturing", transactionID)
			if err != nil {
				return fmt.Errorf("could not update invoice status: %v", err)
			}
		}
	}

	// Commit transaction (automatically releases lock)
	if err := tx.Commit(); err != nil {
		return err
	}

	return nil
}
