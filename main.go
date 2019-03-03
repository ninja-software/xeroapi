package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/XeroAPI/xerogolang"
	"github.com/XeroAPI/xerogolang/accounting"
	"github.com/gofrs/uuid"
	"github.com/markbates/goth"
	"github.com/teris-io/shortid"
	"go.uber.org/ratelimit"

	"git.ninjadojo.com.au/source-energy/hotsource/server/pkg/helpers"
	"git.ninjadojo.com.au/source-energy/hotsource/server/pkg/log"
)

// Notes:
// Xero Get object by searching by parameter, so if need to get id=123, it will have WHERE ID=123, then retrive first record from the response array

// Session stores data during the auth process with Xero.
type Session struct {
	*xerogolang.Session
}

// Provider stores xerogolang library Provider structure
type Provider struct {
	*xerogolang.Provider
}

// Client wraps the Xero API client
type Client struct {
	Provider *xerogolang.Provider
	Sess     goth.Session
	Limiter  ratelimit.Limiter
}

// QueryMap helps filtering records
type QueryMap map[string]string

// New creates a new Xero client
func New(method, key, secret, callbackURL, privateKeyPath, userAgentString string, timeout time.Duration) *Client {
	x := xerogolang.New(key, secret, callbackURL)
	x.PrivateKey = helpers.ReadPrivateKeyFromPath(privateKeyPath)
	x.HTTPClient = &http.Client{Timeout: timeout}
	x.Method = method
	log.Debugln("Xero-ClientKey", x.ClientKey)
	log.Debugln("Xero-Secret", x.Secret)
	log.Debugln("Xero-CallbackURL", x.CallbackURL)
	log.Debugln("Xero-HTTPClient", x.HTTPClient)
	log.Debugln("Xero-Method", x.Method)
	log.Debugln("Xero-UserAgentString", x.UserAgentString)

	sess, err := x.BeginAuth("")
	if err != nil {
		log.Fatalln(err)
	}

	users, err := accounting.FindUsers(x, sess, nil)
	if err != nil {
		log.Fatalln(err)
	}
	fmt.Println(users)
	return &Client{x, sess, ratelimit.New(1)}
}

// customerPost will create a customer(contact), if itemID is not nil, then update customer
func (c *Client) customerPOST(contactID uuid.UUID, name, firstName, lastName, email, status string) (*accounting.Contact, error) {
	// sanity check
	if name == "" {
		return nil, errors.New("name cannot be blank")
	}
	if firstName == "" {
		return nil, errors.New("first name cannot be blank")
	}
	if lastName == "" {
		return nil, errors.New("last name cannot be blank")
	}
	if email == "" {
		return nil, errors.New("email cannot be blank")
	}

	c.Limiter.Take()
	var err error
	var createdContacts *accounting.Contacts

	newContacts := &accounting.Contacts{
		Contacts: []accounting.Contact{
			accounting.Contact{
				Name:         "HS " + name, // TODO, remove HS after dev, added to ease management and remove
				FirstName:    firstName,
				LastName:     lastName,
				EmailAddress: email,
				IsCustomer:   true, // not sure if work, seems like needing invoice to be tied to first
			},
		},
	}

	if status != "" {
		newContacts.Contacts[0].ContactStatus = status
	}

	if contactID != uuid.Nil {
		// update Contact

		newContacts.Contacts[0].ContactID = contactID.String()
		createdContacts, err = newContacts.Update(c.Provider, c.Sess, nil)
	} else {
		// create Contact
		createdContacts, err = newContacts.Create(c.Provider, c.Sess, nil)
	}
	if err != nil {
		return nil, err
	}

	if len(createdContacts.Contacts) != 1 {
		return nil, errors.New("length of Contacts returned did not equal 1")
	}

	createdContact := &createdContacts.Contacts[0]
	return createdContact, nil
}

type NoteRequest struct {
	HistoryRecords []*HistoryRecord `json:"HistoryRecords"`
}
type HistoryRecord struct {
	Details string `json:"Details"`
}

func (c *Client) NoteCreateForContact(xeroContactUUID, body string) error {
	c.Limiter.Take()
	additionalHeaders := map[string]string{
		"Accept":       "application/json",
		"Content-Type": "application/json",
	}

	req := &NoteRequest{}
	req.HistoryRecords = []*HistoryRecord{
		{
			Details: body,
		},
	}
	// payload, err := xml.MarshalIndent(req, "  ", "	")
	// if err != nil {
	// 	return err
	// }

	payload, err := json.Marshal(req)
	if err != nil {
		return err
	}
	fmt.Println(string(payload))

	u, err := url.Parse(fmt.Sprintf("Contacts/%s/history", xeroContactUUID))
	if err != nil {
		return err
	}

	b, err := c.Provider.Create(c.Sess, u.String(), additionalHeaders, payload, nil)
	if err != nil {
		return err
	}
	log.Debugln(string(b))
	return nil
}

// CustomerCreate will create a customer
// Reference: https://developer.xero.com/documentation/api/contacts#POST
func (c *Client) CustomerCreate(name, firstName, lastName, email, meterAddress string, contactAddress *accounting.Address) (*accounting.Contact, error) {
	c.Limiter.Take()
	newContacts := &accounting.Contacts{
		Contacts: []accounting.Contact{
			accounting.Contact{
				Addresses:     &[]accounting.Address{*contactAddress},
				Name:          "HS " + name, // TODO, remove HS after dev, added to ease management and remove
				AccountNumber: meterAddress + " " + shortid.MustGenerate(),
				FirstName:     firstName,
				LastName:      lastName,
				EmailAddress:  email,
				IsCustomer:    true, // not sure if work, seems like needing invoice to be tied to first
			},
		},
	}

	createdContacts, err := newContacts.Create(c.Provider, c.Sess, nil)
	if err != nil {
		return nil, err
	}

	if len(createdContacts.Contacts) != 1 {
		return nil, errors.New("length of contacts returned did not equal 1")
	}

	createdContact := &createdContacts.Contacts[0]
	return createdContact, nil
}

// CustomerUpdate will update a customer
func (c *Client) CustomerUpdate(contactID uuid.UUID, name, firstName, lastName, email, status string) (*accounting.Contact, error) {
	c.Limiter.Take()
	customer, err := c.customerPOST(contactID, name, firstName, lastName, email, status)
	if err != nil {
		return nil, err
	}

	return customer, nil
}

// CustomerList will retrive list of customers (upto 100 at a time), page is the nth(100) of record, starting from page 1
func (c *Client) CustomerList(page int, qm QueryMap) (*[]accounting.Contact, error) {
	c.Limiter.Take()
	query := QueryMap{
		"Page": strconv.Itoa(page),
	}
	// TODO implement qm to query
	for k, v := range qm {
		query[k] = v
	}
	contacts, err := accounting.FindContacts(c.Provider, c.Sess, query)
	if err != nil {
		return nil, err
	}

	return &contacts.Contacts, nil
}

// CustomerGet will retrive a customer
func (c *Client) CustomerGet(customerID uuid.UUID) (*accounting.Contact, error) {
	c.Limiter.Take()
	if customerID == uuid.Nil {
		return nil, errors.New("invalid customer id")
	}
	query := QueryMap{
		// "Where": "IsCustomer=true",
		"IDs": customerID.String(),
	}
	contacts, err := accounting.FindContacts(c.Provider, c.Sess, query)
	if err != nil {
		return nil, err
	}

	if len(contacts.Contacts) != 1 {
		return nil, errors.New("length of contacts returned did not equal 1")
	}

	return &contacts.Contacts[0], nil
}

// CustomerSeedArchive will archive seeded customers
func (c *Client) CustomerSeedArchive() (*[]accounting.Contact, error) {
	c.Limiter.Take()
	query := QueryMap{
		"Where": `Name!=null&&Name.StartsWith("HS ")`,
		"Page":  "1",
	}

	contacts, err := c.CustomerList(1, query)
	if err != nil {
		return nil, err
	}

	for i, contact := range *contacts {
		if i > 30 {
			// TODO, do few at a time now, so wont over flow api limit
			break
		}

		c.CustomerUpdate(uuid.Must(uuid.FromString(contact.ContactID)), contact.Name, contact.FirstName, contact.LastName, contact.EmailAddress, "ARCHIVED")
	}

	contacts2, err := c.CustomerList(1, query)
	if err != nil {
		return nil, err
	}

	return contacts2, nil
}

// AccountCredit will create an invoice
func (c *Client) AccountCredit() (string, error) {
	c.Limiter.Take()
	return "", nil
}

// itemPOST will create an item, if itemID is not nil, then update item
// Reference https://developer.xero.com/documentation/api/items#POST
func (c *Client) itemPOST(itemID uuid.UUID, code, name, desc string, unitPrice float64) (*accounting.Item, error) {
	// sanity check
	if code == "" {
		return nil, errors.New("item code cannot be blank")
	}
	if name == "" {
		return nil, errors.New("item name cannot be blank")
	}
	if desc == "" {
		return nil, errors.New("item description cannot be blank")
	}
	if unitPrice <= 0 {
		return nil, errors.New("unit price must be above 0")
	}

	c.Limiter.Take()
	var err error
	var createdItems *accounting.Items

	newItems := &accounting.Items{
		Items: []accounting.Item{
			accounting.Item{
				Code:        code,
				Name:        name,
				Description: desc, // The sales description of the item (4000 char max)
				SalesDetails: accounting.PurchaseAndSaleDetails{
					UnitPrice: unitPrice,
				},
			},
		},
	}

	// used 4th decimal place if required
	// note: xero will round up the 4th decimal place if the 5th decimal place is >= 5
	qm := QueryMap{}
	if is4dp(unitPrice) {
		qm["unitdp"] = "4"
	}

	if itemID != uuid.Nil {
		// update item
		newItems.Items[0].ItemID = itemID.String()

		createdItems, err = newItems.Update(c.Provider, c.Sess, qm)
	} else {
		// create item

		createdItems, err = newItems.Create(c.Provider, c.Sess, qm)
	}
	if err != nil {
		return nil, err
	}

	if len(createdItems.Items) != 1 {
		return nil, errors.New("length of items returned did not equal 1")
	}

	createdItem := &createdItems.Items[0]
	return createdItem, nil
}

// ItemCreate will create an item
func (c *Client) ItemCreate(code, name, desc string, unitPrice float64) (*accounting.Item, error) {
	c.Limiter.Take()
	item, err := c.itemPOST(uuid.Nil, code, name, desc, unitPrice)
	if err != nil {
		return nil, err
	}

	return item, nil
}

// ItemUpdate will update an item
func (c *Client) ItemUpdate(itemID uuid.UUID, code, name, desc string, unitPrice float64) (*accounting.Item, error) {
	c.Limiter.Take()
	item, err := c.itemPOST(itemID, code, name, desc, unitPrice)
	if err != nil {
		return nil, err
	}

	return item, nil
}

// ItemList will retrive list of items (upto 100 at a time), page is the nth(100) of record, starting from page 1
func (c *Client) ItemList(page int, qm QueryMap) (*[]accounting.Item, error) {
	c.Limiter.Take()
	qm["Page"] = strconv.Itoa(page)
	qm["unitdp"] = "4" // get upto 4th decimal place for unit prices

	items, err := accounting.FindItems(c.Provider, c.Sess, qm)
	if err != nil {
		return nil, err
	}

	return &items.Items, nil
}

// ItemGet will retrive an item
func (c *Client) ItemGet(itemID uuid.UUID) (*accounting.Item, error) {
	c.Limiter.Take()
	if itemID == uuid.Nil {
		return nil, errors.New("invalid item id")
	}

	query := QueryMap{
		"IDs":    itemID.String(),
		"unitdp": "4",
	}
	items, err := accounting.FindItems(c.Provider, c.Sess, query)
	if err != nil {
		return nil, err
	}

	if len(items.Items) != 1 {
		return nil, errors.New("length of contacts returned did not equal 1")
	}

	return &items.Items[0], nil
}

// invoicePOST will create an invoice, if invoiceID is not nil, then update invoice
// Reference https://developer.xero.com/documentation/api/invoices#post
func (c *Client) invoicePOST(invoiceID uuid.UUID, invoiceNumber, reference string, contactID uuid.UUID, date, dueDate time.Time, lineItems []accounting.LineItem) (*accounting.Invoice, error) {
	// sanity check
	if contactID == uuid.Nil {
		return nil, errors.New("contact id cannot be blank")
	}
	if date.IsZero() {
		return nil, errors.New("date cannot be zero")
	}
	if dueDate.IsZero() {
		return nil, errors.New("due date cannot be zero")
	}
	if date.After(dueDate) {
		return nil, errors.New("date cannot be after due date")
	}

	c.Limiter.Take()
	var err error
	var createdInvoices *accounting.Invoices

	newInvoices := &accounting.Invoices{
		Invoices: []accounting.Invoice{
			accounting.Invoice{
				Type:    "ACCREC", // accounts receival aka customer invoice
				Date:    toYMD(date),
				DueDate: toYMD(dueDate),
				Contact: accounting.Contact{
					ContactID: contactID.String(),
				},
				InvoiceNumber: invoiceNumber, // ACCREC - Unique alpha numeric code identifying invoice ( when missing will auto-generate from your Organisation Invoice Settings) (max length = 255)
				Reference:     reference,     // ACCREC only - additional reference number (max length = 255)
				Status:        "AUTHORISED",
				LineItems:     lineItems,
			},
		},
	}

	// optional query goes here, for e.g. unitdp
	qm := QueryMap{}

	if invoiceID != uuid.Nil {
		// update invoice
		newInvoices.Invoices[0].InvoiceID = invoiceID.String()

		createdInvoices, err = newInvoices.Update(c.Provider, c.Sess, qm)
	} else {
		// create invoice

		createdInvoices, err = newInvoices.Create(c.Provider, c.Sess, qm)
	}
	if err != nil {
		return nil, err
	}

	if len(createdInvoices.Invoices) != 1 {
		return nil, errors.New("length of invoices returned did not equal 1")
	}

	createdInvoice := &createdInvoices.Invoices[0]
	return createdInvoice, nil
}

// InvoiceCreate will create an invoice
func (c *Client) InvoiceCreate(invoiceNumber, reference string, contactID uuid.UUID, date, dueDate time.Time, lineItems []accounting.LineItem) (*accounting.Invoice, error) {
	c.Limiter.Take()
	invoice, err := c.invoicePOST(uuid.Nil, invoiceNumber, reference, contactID, date, dueDate, lineItems)
	if err != nil {
		return nil, err
	}

	return invoice, nil
}

// InvoiceUpdate will update an invoice
func (c *Client) InvoiceUpdate(invoiceID uuid.UUID, invoiceNumber, reference string, contactID uuid.UUID, date, dueDate time.Time, lineItems []accounting.LineItem) (*accounting.Invoice, error) {
	c.Limiter.Take()
	// Note
	// the old LineItems will be nuked and created from scratch
	// if old LineItems wanted to be kept, the LineItem must contain LineItemID

	invoice, err := c.invoicePOST(invoiceID, invoiceNumber, reference, contactID, date, dueDate, lineItems)
	if err != nil {
		return nil, err
	}

	return invoice, nil
}

// InvoiceList will retrive list of invoices (upto 100 at a time), page is the nth(100) of record, starting from page 1
func (c *Client) InvoiceList(page int, qm QueryMap) (*[]accounting.Invoice, error) {
	c.Limiter.Take()
	// TODO implement query handler
	if page > 0 {
		qm["Page"] = strconv.Itoa(page)
	}
	qm["unitdp"] = "4" // get upto 4th decimal place for unit prices
	qm["order"] = "DueDate DESC"

	invoices, err := accounting.FindInvoices(c.Provider, c.Sess, qm)
	if err != nil {
		return nil, err
	}

	return &invoices.Invoices, nil
}

// InvoiceGet will retrive an invoice
func (c *Client) InvoiceGet(invoiceID uuid.UUID) (*accounting.Invoice, error) {
	c.Limiter.Take()
	if invoiceID == uuid.Nil {
		return nil, errors.New("invalid invoice id")
	}

	query := QueryMap{
		"IDs":    invoiceID.String(),
		"unitdp": "4",
	}
	invoices, err := accounting.FindInvoices(c.Provider, c.Sess, query)
	if err != nil {
		return nil, err
	}

	if len(invoices.Invoices) != 1 {
		return nil, errors.New("length of contacts returned did not equal 1")
	}

	return &invoices.Invoices[0], nil
}

// is4dp check if price should be in 4 decimal place
func is4dp(price float64) bool {
	result := false

	strPrice1 := fmt.Sprintf("%0.4f", price)
	strPrice2 := fmt.Sprintf("%.2f", price)
	price1, _ := strconv.ParseFloat(strPrice1, 64)
	price2, _ := strconv.ParseFloat(strPrice2, 64)

	price3 := price1 - price2

	if price3 > 0 {
		result = true
	}
	return result
}

// PaymentOne is to combine all 4 different xero payments into one same struct to make it easier to handle
type PaymentOne struct {
}

// PaymentCreate will create payment
// Reference https://developer.xero.com/documentation/api/payments
func (c *Client) PaymentCreate(invoiceID uuid.UUID, date time.Time, amount float64, reference string, accountCode string, qm QueryMap) (*accounting.Payment, error) {
	// sanity check
	if invoiceID == uuid.Nil {
		return nil, errors.New("invoice id cannot be blank")
	}
	if amount <= 0 {
		return nil, errors.New("amount must be above 0")
	}
	if reference == "" {
		return nil, errors.New("reference cannot be blank")
	}
	if date.IsZero() {
		return nil, errors.New("invalid date zero")
	}
	if accountCode == "" {
		return nil, errors.New("account code cannot be blank")
	}

	c.Limiter.Take()
	newPayments := accounting.Payments{
		Payments: []accounting.Payment{
			accounting.Payment{
				Date:        toYMD(date), // Date the payment is being made (YYYY-MM-DD) e.g. 2009-09-06
				Amount:      amount,      // The amount of the payment. Must be less than or equal to the outstanding amount owing on the invoice e.g. 200.00
				Reference:   reference,   // An optional description for the payment e.g. Direct Debit
				Status:      "AUTHORISED",
				PaymentType: "ACCRECPAYMENT",
				Invoice: &accounting.Invoice{
					InvoiceID: invoiceID.String(),
				},
				Account: &accounting.Account{
					Code: accountCode,
				},
			},
		},
	}

	payments, err := newPayments.Create(c.Provider, c.Sess, qm)
	if err != nil {
		return nil, err
	}

	if len(payments.Payments) != 1 {
		return nil, errors.New("length of Payments returned did not equal 1")
	}

	return &payments.Payments[0], nil
}

// PaymentList will list payments
func (c *Client) PaymentList(page int, qm QueryMap) (*[]accounting.Payment, error) {
	c.Limiter.Take()
	// TODO implement query handler
	qm["Page"] = strconv.Itoa(page)

	payments, err := accounting.FindPayments(c.Provider, c.Sess, qm)
	if err != nil {
		return nil, err
	}

	// prepayments, err := accounting.FindPrepayments(c.Provider, c.Sess, qm)
	// if err != nil {
	// 	return nil, err
	// }
	// spew.Dump(prepayments)

	// overpayments, err := accounting.FindOverpayments(c.Provider, c.Sess, qm)
	// if err != nil {
	// 	return nil, err
	// }
	// spew.Dump(overpayments)

	// creditnotes, err := accounting.FindCreditNotes(c.Provider, c.Sess, qm)
	// if err != nil {
	// 	return nil, err
	// }
	// spew.Dump(creditnotes)

	// TODO turn this dummy to real
	// dummy := accounting.Payments{}

	return &payments.Payments, nil
}
