package checkoutsync

import (
	"fmt"
	"net/http"
	"time"
)

func (c *checkoutSync) Feedback() (string, error) {
	if err := c.ready(); err != nil {
		return "", err
	}
	if feedback := c.checkHealth(); feedback != "" {
		return feedback, nil
	}
	feedback, _ := c.runScenarios(feedbackScenarios())
	return feedback, nil
}

func (c *checkoutSync) Validation() (bool, error) {
	if err := c.ready(); err != nil {
		return false, err
	}
	_, passed := c.runScenarios(validationScenarios())
	return passed, nil
}

func (c *checkoutSync) checkHealth() string {
	client := &http.Client{Timeout: 5 * time.Second}
	response, err := client.Get(origin(c.applicationPort) + "/health")
	if err != nil {
		return fmt.Sprintf("Check %q failed. Diagnosis: GET /health failed: %v", "application health", err)
	}
	_ = response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Sprintf("Check %q failed. Expected a 2xx response, got HTTP %d.", "application health", response.StatusCode)
	}
	return ""
}

func stringBody(email, first, last, amount, currency string) string {
	return fmt.Sprintf(`{"email":%q,"firstName":%q,"lastName":%q,"amount":%s,"currency":%q}`, email, first, last, amount, currency)
}

// checkoutThen runs a valid checkout and, on success, an optional follow-up.
func checkoutThen(spec checkoutSpec, want outcome, then func(e *env, orderID, contactID string) string) func(e *env) string {
	return func(e *env) string {
		orderID, contactID, diagnosis := e.expectCheckout(spec, want)
		if diagnosis != "" || then == nil {
			return diagnosis
		}
		return then(e, orderID, contactID)
	}
}

func getAfter(spec checkoutSpec, status string) func(e *env, orderID, contactID string) string {
	return func(e *env, orderID, contactID string) string {
		return e.expectOrder(orderID, orderDocument(orderID, contactID, spec, status))
	}
}

func rejected(name, body string) scenario {
	return scenario{name: name, run: func(e *env) string { return e.expectRejected(body) }}
}

func feedbackScenarios() []scenario {
	newContact := checkoutSpec{
		body:  stringBody("ada@example.com", "Ada", "Lovelace", `19.99`, "USD"),
		email: "ada@example.com", firstName: "Ada", lastName: "Lovelace", amount: "19.99", currency: "USD",
	}
	existingContact := checkoutSpec{
		body:  stringBody("grace@example.com", "Grace", "Hopper", `"42.50"`, "EUR"),
		email: "grace@example.com", firstName: "Grace", lastName: "Hopper", amount: "42.50", currency: "EUR",
	}
	failing := checkoutSpec{
		body:  stringBody("alan@example.com", "Alan", "Turing", `5`, "GBP"),
		email: "alan@example.com", firstName: "Alan", lastName: "Turing", amount: "5.00", currency: "GBP",
	}
	return []scenario{
		{
			name: "new contact is created, order captured and payment stored",
			run:  checkoutThen(newContact, outcomeCompleted, getAfter(newContact, "COMPLETED")),
		},
		{
			name:  "existing contact is updated instead of created",
			seeds: []seedContact{{ID: "501", Email: "grace@example.com", FirstName: "G.", LastName: "H."}},
			run:   checkoutThen(existingContact, outcomeCompleted, getAfter(existingContact, "COMPLETED")),
		},
		{
			name:          "failed capture returns 502 and stores a FAILED row",
			captureStatus: http.StatusUnprocessableEntity,
			run:           checkoutThen(failing, outcomeCaptureFailed, getAfter(failing, "FAILED")),
		},
		{
			name: "unknown order returns 404",
			run:  func(e *env) string { return e.expectNotFound("NO-SUCH-ORDER") },
		},
		rejected("missing amount returns 400", `{"email":"ada@example.com","firstName":"Ada","lastName":"Lovelace","currency":"USD"}`),
		rejected("malformed JSON returns 400", `{"email":"ada@example.com"`),
	}
}

func validationScenarios() []scenario {
	unicodeSpec := checkoutSpec{
		body:  `{ "currency" : "EUR", "lastName": "Müller-Åström", "amount": 1.2e1, "note": "ignored", "firstName": "Zoë", "email": "zoe.muller@example.org" }`,
		email: "zoe.muller@example.org", firstName: "Zoë", lastName: "Müller-Åström", amount: "12.00", currency: "EUR",
	}
	smallSpec := checkoutSpec{
		body:  stringBody("small@example.net", "Sam", "Small", `"0.05"`, "CAD"),
		email: "small@example.net", firstName: "Sam", lastName: "Small", amount: "0.05", currency: "CAD",
	}
	mixedCaseSpec := checkoutSpec{
		body:  stringBody("Existing.User@Example.com", "Edith", "Clarke", `1234567.8`, "USD"),
		email: "Existing.User@Example.com", firstName: "Edith", lastName: "Clarke", amount: "1234567.80", currency: "USD",
	}
	firstRepeat := checkoutSpec{
		body:  stringBody("repeat@example.com", "Rita", "Repeat", `10.00`, "USD"),
		email: "repeat@example.com", firstName: "Rita", lastName: "Repeat", amount: "10.00", currency: "USD",
	}
	secondRepeat := checkoutSpec{
		body:  stringBody("repeat@example.com", "Rita", "Renamed", `"20.5"`, "USD"),
		email: "repeat@example.com", firstName: "Rita", lastName: "Renamed", amount: "20.50", currency: "USD",
	}
	createFails := checkoutSpec{
		body:  stringBody("nocreate@example.com", "Nora", "Nocreate", `7.77`, "AUD"),
		email: "nocreate@example.com", firstName: "Nora", lastName: "Nocreate", amount: "7.77", currency: "AUD",
	}
	captureFails := checkoutSpec{
		body:  stringBody("nocapture@example.com", "Cleo", "Nocapture", `99`, "SEK"),
		email: "nocapture@example.com", firstName: "Cleo", lastName: "Nocapture", amount: "99.00", currency: "SEK",
	}
	seededRow := paymentRow{
		OrderID: "HIDDEN-ORDER-1", ContactID: "9100", Email: "hidden@example.com", Amount: "310.25", Currency: "CHF", Status: "FAILED",
	}
	return []scenario{
		{
			name: "hidden: new contact with reordered fields, exponent amount and unicode names",
			run:  checkoutThen(unicodeSpec, outcomeCompleted, getAfter(unicodeSpec, "COMPLETED")),
		},
		{
			name: "hidden: small string amount",
			run:  checkoutThen(smallSpec, outcomeCompleted, getAfter(smallSpec, "COMPLETED")),
		},
		{
			name:  "hidden: existing contact found regardless of email case",
			seeds: []seedContact{{ID: "7001", Email: "existing.user@example.com", FirstName: "Old", LastName: "Name"}, {ID: "7002", Email: "other@example.com", FirstName: "Other", LastName: "Person"}},
			run:   checkoutThen(mixedCaseSpec, outcomeCompleted, getAfter(mixedCaseSpec, "COMPLETED")),
		},
		{
			name: "hidden: second checkout for the same email reuses the contact",
			run: func(e *env) string {
				firstOrder, firstContact, diagnosis := e.expectCheckout(firstRepeat, outcomeCompleted)
				if diagnosis != "" {
					return diagnosis
				}
				secondOrder, secondContact, diagnosis := e.expectCheckout(secondRepeat, outcomeCompleted)
				if diagnosis != "" {
					return diagnosis
				}
				if firstOrder == secondOrder || firstContact != secondContact {
					return fmt.Sprintf("expected two distinct orders for one contact, got orders %q/%q contacts %q/%q", firstOrder, secondOrder, firstContact, secondContact)
				}
				if diagnosis := e.expectOrder(firstOrder, orderDocument(firstOrder, firstContact, firstRepeat, "COMPLETED")); diagnosis != "" {
					return diagnosis
				}
				return e.expectOrder(secondOrder, orderDocument(secondOrder, secondContact, secondRepeat, "COMPLETED"))
			},
		},
		{
			name:         "hidden: failed order creation returns 502 and stores nothing",
			createStatus: http.StatusInternalServerError,
			run:          checkoutThen(createFails, outcomeCreateFailed, nil),
		},
		{
			name:          "hidden: failed capture for an existing contact stores a FAILED row",
			seeds:         []seedContact{{ID: "7100", Email: "nocapture@example.com", FirstName: "C.", LastName: "N."}},
			captureStatus: http.StatusInternalServerError,
			run:           checkoutThen(captureFails, outcomeCaptureFailed, getAfter(captureFails, "FAILED")),
		},
		{
			name:    "hidden: order is read from the database",
			preload: []paymentRow{seededRow},
			run: func(e *env) string {
				return e.expectOrder(seededRow.OrderID, map[string]any{
					"orderId": seededRow.OrderID, "contactId": seededRow.ContactID, "email": seededRow.Email,
					"amount": 310.25, "currency": seededRow.Currency, "status": seededRow.Status,
				})
			},
		},
		{
			name:    "hidden: unknown order returns 404",
			preload: []paymentRow{seededRow},
			run:     func(e *env) string { return e.expectNotFound("HIDDEN-ORDER-2") },
		},
		rejected("hidden: empty body", ``),
		rejected("hidden: JSON array", `[]`),
		rejected("hidden: missing email", `{"firstName":"A","lastName":"B","amount":1,"currency":"USD"}`),
		rejected("hidden: email without at sign", stringBody("not-an-email", "A", "B", `1`, "USD")),
		rejected("hidden: empty first name", stringBody("a@example.com", "", "B", `1`, "USD")),
		rejected("hidden: non-string last name", `{"email":"a@example.com","firstName":"A","lastName":7,"amount":1,"currency":"USD"}`),
		rejected("hidden: zero amount", stringBody("a@example.com", "A", "B", `0`, "USD")),
		rejected("hidden: negative amount", stringBody("a@example.com", "A", "B", `-5.00`, "USD")),
		rejected("hidden: non-numeric amount string", stringBody("a@example.com", "A", "B", `"abc"`, "USD")),
		rejected("hidden: boolean amount", stringBody("a@example.com", "A", "B", `true`, "USD")),
		rejected("hidden: three fractional digits", stringBody("a@example.com", "A", "B", `1.234`, "USD")),
		rejected("hidden: short currency", stringBody("a@example.com", "A", "B", `1`, "US")),
		rejected("hidden: lower-case currency", stringBody("a@example.com", "A", "B", `1`, "usd")),
		rejected("hidden: missing currency", `{"email":"a@example.com","firstName":"A","lastName":"B","amount":1}`),
	}
}
