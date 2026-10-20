import ballerina/ai;
import ballerina/http;
import ballerina/log;
import ballerinax/ai.openai;
import ballerinax/googleapis.gmail;
import ballerinax/hubspot.crm.obj.contacts;
import ballerinax/kafka;

configurable string enrichUrl = "http://enrich:8080";
configurable string hubspotUrl = "http://hubspot:8080/crm/v3/objects/contacts";
configurable string gmailUrl = "http://gmail:8080/gmail/v1";
configurable string openaiUrl = "http://openai:8080/v1";
configurable string kafkaServer = "kafka:9092";
configurable string apiToken = "test-token";
configurable string openaiKey = "test-key";

const LEADS_TOPIC = "leads";
const HOT = "HOT";

type LeadRequest record {|
    string email;
    string firstName;
    string lastName;
    string company;
    string message;
|};

type Enrichment record {
    string industry;
};

type LeadEvent record {|
    string contactId;
    string email;
    string rating;
|};

type LeadResponse record {|
    string contactId;
    string rating;
|};

final http:Client enrichClient = check new (enrichUrl);
final contacts:Client hubspotClient = check new ({auth: {token: apiToken}}, serviceUrl = hubspotUrl);
final gmail:Client gmailClient = check new ({auth: {token: apiToken}}, serviceUrl = gmailUrl);
final openai:ModelProvider model = check new (openaiKey, openai:GPT_4O_MINI, serviceUrl = openaiUrl);
final kafka:Producer leadProducer = check new (kafkaServer);

service / on new http:Listener(8090) {
    resource function post leads(@http:Payload json payload) returns http:Created|http:BadRequest|error {
        LeadRequest|error lead = payload.cloneWithType();
        if lead is error || !isValid(lead) {
            return http:BAD_REQUEST;
        }
        string industry = check lookupIndustry(lead.company);
        string rating = check classify(lead.message);
        string contactId = check upsertContact(lead, industry, rating);
        LeadEvent event = {contactId, email: lead.email, rating};
        check leadProducer->send({topic: LEADS_TOPIC, value: event});
        http:Created created = {body: <LeadResponse>{contactId, rating}};
        return created;
    }
}

function isValid(LeadRequest lead) returns boolean {
    foreach string text in [lead.email, lead.firstName, lead.lastName, lead.company, lead.message] {
        if text.trim().length() == 0 {
            return false;
        }
    }
    return true;
}

function lookupIndustry(string company) returns string|error {
    http:Response response = check enrichClient->get(string `/companies/${company}`);
    if response.statusCode == http:STATUS_NOT_FOUND {
        return "Unknown";
    }
    Enrichment enrichment = check (check response.getJsonPayload()).cloneWithType();
    return enrichment.industry;
}

function classify(string message) returns string|error {
    ai:ChatAssistantMessage reply = check model->chat({
        role: ai:USER,
        content: string `Classify this sales lead message as HOT or COLD. Answer with one word.\n\n${message}`
    });
    string answer = (reply.content ?: "").trim().toUpperAscii();
    return answer.startsWith(HOT) ? HOT : "COLD";
}

function upsertContact(LeadRequest lead, string industry, string rating) returns string|error {
    contacts:CollectionResponseWithTotalSimplePublicObjectForwardPaging found = check hubspotClient->/search.post({
        filterGroups: [{filters: [{propertyName: "email", operator: "EQ", value: lead.email}]}],
        properties: ["email"]
    });
    map<string> properties = {
        firstname: lead.firstName,
        lastname: lead.lastName,
        company: lead.company,
        industry,
        lead_rating: rating
    };
    if found.results.length() > 0 {
        string contactId = found.results[0].id;
        _ = check hubspotClient->/[contactId].patch({properties});
        return contactId;
    }
    properties["email"] = lead.email;
    contacts:SimplePublicObject created = check hubspotClient->/.post({properties});
    return created.id;
}

listener kafka:Listener leadListener = new (kafkaServer, {
    groupId: "lead-mailer",
    topics: [LEADS_TOPIC],
    offsetReset: kafka:OFFSET_RESET_EARLIEST
});

service on leadListener {
    remote function onConsumerRecord(LeadEvent[] events) returns error? {
        foreach LeadEvent event in events {
            if event.rating != HOT {
                continue;
            }
            gmail:Message sent = check gmailClient->/users/["me"]/messages/send.post({
                to: ["sales@example.com"],
                subject: string `Hot lead: ${event.email}`,
                bodyInText: string `New hot lead ${event.email}, HubSpot contact ${event.contactId}.`
            });
            log:printInfo("hot lead mailed", contactId = event.contactId, messageId = sent.id);
        }
    }
}
