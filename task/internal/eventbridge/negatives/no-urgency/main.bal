import ballerina/lang.regexp;
import ballerina/lang.value;
import ballerina/log;
import ballerinax/nats;
import ballerinax/pagerduty;
import ballerinax/rabbitmq;

const INCOMING_QUEUE = "orders.incoming";
const DEAD_EXCHANGE = "orders.dlx";
const DEAD_QUEUE = "orders.dead";
const STREAM_NAME = "ORDERS";
const STREAM_SUBJECTS = "orders.created.*";
const SUBJECT_PREFIX = "orders.created.";
const SECONDS_PER_DAY = 86400d;
const MAX_STORED_EVENTS = 100000.0;
const POISON_THRESHOLD = 3;
const PAGERDUTY_SERVICE = "PSVC001";
const PAGERDUTY_FROM = "ops@example.com";
const PAGERDUTY_VERSION_HEADER = "application/vnd.pagerduty+json;version=2";
const JSON_CONTENT_TYPE = "application/json";

final regexp:RegExp currencyPattern = re `[A-Z]{3}`;

type OrderEvent record {
    string orderId;
    string customerId;
    decimal amount;
    string currency;
};

final nats:Client natsClient = check new ("nats://nats:4222");
final nats:JetStreamClient streams = check new (natsClient);
final pagerduty:Client pager = check new ({authorization: "Token token=pd-test-key"}, serviceUrl = "http://pagerduty:8080");

isolated int poisonCount = 0;

listener rabbitmq:Listener broker = new ("rabbitmq", 5672, username = "guest", password = "guest");

function init() returns error? {
    check streams->addStream({
        name: STREAM_NAME,
        subjects: [STREAM_SUBJECTS],
        storageType: nats:FILE,
        maxAge: SECONDS_PER_DAY,
        maxMsgs: MAX_STORED_EVENTS
    });
}

@rabbitmq:ServiceConfig {
    queueName: INCOMING_QUEUE,
    autoAck: false,
    config: {
        durable: true,
        arguments: {"x-dead-letter-exchange": DEAD_EXCHANGE, "x-dead-letter-routing-key": DEAD_QUEUE}
    }
}
service rabbitmq:Service on broker {
    remote function onMessage(rabbitmq:BytesMessage message, rabbitmq:Caller caller) returns error? {
        OrderEvent|error event = parse(message.content);
        if event is error {
            check caller->basicNack(false, false);
            error? alerted = reportPoison(event.message());
            if alerted is error {
                log:printError("PagerDuty incident failed", alerted);
            }
            return;
        }
        check streams->publishMessage({subject: SUBJECT_PREFIX + event.currency.toLowerAscii(), content: message.content});
        check caller->basicAck();
    }
}

function parse(byte[] content) returns OrderEvent|error {
    json payload = check value:fromJsonString(check string:fromBytes(content));
    OrderEvent event = check payload.cloneWithType();
    if event.orderId.trim() == "" || event.customerId.trim() == "" || event.amount <= 0d
            || !currencyPattern.isFullMatch(event.currency) {
        return error("order event breaks the rules");
    }
    return event;
}

function reportPoison(string reason) returns error? {
    int total;
    lock {
        poisonCount += 1;
        total = poisonCount;
    }
    log:printWarn("poison message dead-lettered", reason = reason, total = total);
    if total != POISON_THRESHOLD {
        return;
    }
    _ = check pager->createIncident(PAGERDUTY_VERSION_HEADER, JSON_CONTENT_TYPE, PAGERDUTY_FROM, {
        incident: {
            'type: "incident",
            title: string `Order bridge: ${total} poison messages dead-lettered`,
            'service: {"id": PAGERDUTY_SERVICE, 'type: "service_reference"}
        }
    });
}
