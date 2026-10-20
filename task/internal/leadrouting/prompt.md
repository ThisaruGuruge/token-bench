Build a Ballerina integration in the current directory. Create the Ballerina project here, so that `Ballerina.toml` is in this directory and `bal run` from this directory starts the program. Use these libraries from Ballerina Central: `ballerina/http`, `ballerinax/kafka`, `ballerinax/hubspot.crm.obj.contacts`, `ballerinax/googleapis.gmail`, and `ballerinax/ai.openai` (an OpenAI model provider for `ballerina/ai`).

The external systems are already running and reachable from here. Use exactly these values:

| System | Value |
|---|---|
| Company enrichment API (plain HTTP, no auth) | `http://enrich:8080` |
| HubSpot contacts API | base URL `http://hubspot:8080/crm/v3/objects/contacts`, bearer token `test-token` |
| Gmail API | base URL `http://gmail:8080/gmail/v1`, bearer token `test-token` |
| OpenAI-compatible model API | base URL `http://openai:8080/v1`, API key `test-key`, model GPT-4o mini |
| Kafka bootstrap server | `kafka:9092` |

## `POST /leads` on port 8090

The request body is a JSON object:

```json
{"email": "ada@example.com", "firstName": "Ada", "lastName": "Lovelace", "company": "Acme Corp", "message": "Can we get a quote?"}
```

All five fields are required non-empty strings. Reject invalid JSON, or a body with a missing or empty field, with HTTP 400, and call no external system for it.

For a valid lead, in this order:

1. **Enrichment.** `GET http://enrich:8080/companies/{company}` answers `{"company": "...", "industry": "...", "employees": 1200}`. If it answers 404, the industry is `Unknown`.
2. **Rating.** Ask the model to classify the lead's `message` as `HOT` or `COLD`. The model's answer is the rating.
3. **HubSpot.** Search contacts for the email (an `EQ` filter on the `email` property). If a contact exists, update it setting the properties `firstname`, `lastname`, `company`, `industry` and `lead_rating` (the rating). Otherwise create a contact with the properties `email`, `firstname`, `lastname`, `company`, `industry` and `lead_rating`. Never create a second contact for an email that already has one. HubSpot contact ids are strings.
4. **Kafka.** Publish one record to the topic `leads` whose value is the JSON object `{"contactId": "...", "email": "...", "rating": "HOT"}`.
5. Respond with HTTP 201 and `{"contactId": "<HubSpot contact id>", "rating": "HOT"}` (or `"COLD"`).

## Hot-lead mailer

In the same program, a Kafka consumer with group id `lead-mailer` reads the topic `leads`. For every record whose rating is `HOT`, it sends one email through Gmail, as the user `me`, to `sales@example.com`, with the subject `Hot lead: <email>` and a body that contains the contact id. `COLD` leads get no email.

## Done means

`bal build` succeeds in this directory. You may start the program and try it against the systems above, but stop it before you finish. Do not write tests.
