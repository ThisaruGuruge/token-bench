// Package discoverqa holds the discovery-only question tasks of the grammar
// benchmark: one question about a Ballerina library's API per task, answered
// in answer.json and graded field by field.
package discoverqa

// Field kinds decide how a field is normalised before comparison.
const (
	kindName    = "name"    // an identifier
	kindType    = "type"    // a Ballerina type
	kindPath    = "path"    // a resource path
	kindValue   = "value"   // a default value (numbers compared numerically)
	kindLibrary = "library" // an org/name coordinate
	kindInt     = "int"     // a count
	kindNames   = "names"   // a set of identifiers
	kindChain   = "chain"   // an ordered list of identifiers
	kindTypes   = "types"   // an ordered list of types
	kindParams  = "params"  // an ordered list of {name, type[, default]}
)

// Param is one expected parameter. Default is checked only when non-empty.
type Param struct {
	Name    string
	Type    string
	Default string
}

// Field is one graded field of an answer.
type Field struct {
	Key  string
	Kind string
	// Want holds the accepted answers for scalar kinds (any one matches) and
	// the expected members for kindNames and kindTypes.
	Want   []string
	Params []Param
}

// Definition is one question task.
type Definition struct {
	ID       string
	Category string
	Question string
	// Shape is the answer.json skeleton shown to the agent.
	Shape  string
	Fields []Field
	// FirstCommands lists, per grammar ("A", "B"), the argv (after
	// `bal discover`) of an on-target first command.
	FirstCommands map[string][][]string
}

// Frame is the instruction every question is posed with.
const Frame = "Answer the question below about a Ballerina library's API. Look it up with the tools available; " +
	"do not answer from memory. Do not create a project or write code. Write your answer to `answer.json` in the " +
	"current directory, in exactly the shape shown, then stop. Write Ballerina types as the API declares them; a " +
	"module prefix such as `kafka:` is optional."

func one(values ...string) []string { return values }

// Tasks are the 24 questions, in ID order. Versions are pinned except those of
// D16, D19 and D20, which the cache seed pins through its latest-version entries.
var Tasks = []Definition{
	{
		ID: "D01", Category: "API discovery",
		Question: "In ballerinax/kafka version 4.6.6, list every remote operation (the ones called with `->`) of the Kafka producer client.",
		Shape:    `{"operations": ["<name>", "..."]}`,
		Fields:   []Field{{Key: "operations", Kind: kindNames, Want: one("close", "flush", "getTopicPartitions", "send", "sendWithMetadata")}},
		FirstCommands: map[string][][]string{
			"A": {{"ballerinax/kafka:4.6.6", "client", "Producer"}},
			"B": {{"method", "-p", "ballerinax/kafka:4.6.6", "--client", "Producer"}},
		},
	},
	{
		ID: "D02", Category: "Call with right types",
		Question: "In ballerinax/kafka version 4.6.6, you want to publish a record to a topic with the producer. Which operation do you call, what is the type of the record you pass to it, which fields of that record are required, and what does the operation return?",
		Shape:    `{"operation": "<name>", "recordType": "<type>", "requiredFields": ["<field>", "..."], "returnType": "<type>"}`,
		Fields: []Field{
			{Key: "operation", Kind: kindName, Want: one("send")},
			{Key: "recordType", Kind: kindType, Want: one("AnydataProducerRecord")},
			{Key: "requiredFields", Kind: kindNames, Want: one("topic", "value")},
			{Key: "returnType", Kind: kindType, Want: one("Error?")},
		},
		FirstCommands: map[string][][]string{
			"A": {{"ballerinax/kafka:4.6.6", "client", "send"}, {"ballerinax/kafka:4.6.6", "client", "Producer", "send"}},
			"B": {{"method", "send", "-p", "ballerinax/kafka:4.6.6"}, {"method", "send", "-p", "ballerinax/kafka:4.6.6", "--client", "Producer"}},
		},
	},
	{
		ID: "D03", Category: "Ambiguous name",
		Question: "In ballerinax/kafka version 4.6.6, the consumer client has a `getTopicPartitions` operation. Give its parameters in order (name, type and default value if it has one) and its return type.",
		Shape:    `{"parameters": [{"name": "<name>", "type": "<type>", "default": "<default value, or omit the key>"}], "returnType": "<type>"}`,
		Fields: []Field{
			{Key: "parameters", Kind: kindParams, Params: []Param{{Name: "topic", Type: "string"}, {Name: "duration", Type: "decimal", Default: "-1"}}},
			{Key: "returnType", Kind: kindType, Want: one("TopicPartition[]|Error")},
		},
		FirstCommands: map[string][][]string{
			"A": {{"ballerinax/kafka:4.6.6", "client", "Consumer", "getTopicPartitions"}},
			"B": {{"method", "getTopicPartitions", "-p", "ballerinax/kafka:4.6.6", "--client", "Consumer"}},
		},
	},
	{
		ID: "D04", Category: "Service + listener",
		Question: "In ballerinax/kafka version 4.6.6, a service consumes records from Kafka. Which handlers must such a service implement, which may it implement, and what is the listener type it attaches to?",
		Shape:    `{"requiredHandlers": ["<name>", "..."], "optionalHandlers": ["<name>", "..."], "listenerType": "<type>"}`,
		Fields: []Field{
			{Key: "requiredHandlers", Kind: kindNames, Want: one("onConsumerRecord")},
			{Key: "optionalHandlers", Kind: kindNames, Want: one("onError")},
			{Key: "listenerType", Kind: kindType, Want: one("Listener")},
		},
		FirstCommands: map[string][][]string{
			"A": {{"ballerinax/kafka:4.6.6", "service", "Service"}},
			"B": {{"service", "Service", "-p", "ballerinax/kafka:4.6.6"}},
		},
	},
	{
		ID: "D05", Category: "Call with right types",
		Question: "In ballerina/http version 2.16.6, the HTTP client has an operation that sends a POST request. Give its name, its parameters in order (name and type) and its return type.",
		Shape:    `{"operation": "<name>", "parameters": [{"name": "<name>", "type": "<type>"}], "returnType": "<type>"}`,
		Fields: []Field{
			{Key: "operation", Kind: kindName, Want: one("post")},
			{Key: "parameters", Kind: kindParams, Params: []Param{
				{Name: "path", Type: "string"}, {Name: "message", Type: "RequestMessage"},
				{Name: "headers", Type: "map<string|string[]>?"}, {Name: "mediaType", Type: "string?"},
				{Name: "targetType", Type: "TargetType"},
			}},
			{Key: "returnType", Kind: kindType, Want: one("targetType|ClientError")},
		},
		FirstCommands: map[string][][]string{
			"A": {{"ballerina/http:2.16.6", "client", "Client", "post"}, {"ballerina/http:2.16.6", "client", "post"}},
			"B": {{"method", "post", "-p", "ballerina/http:2.16.6", "--client", "Client"}, {"method", "post", "-p", "ballerina/http:2.16.6"}},
		},
	},
	{
		ID: "D06", Category: "Type/defaults",
		Question: "In ballerina/http version 2.16.6, what is the name of the record that configures the HTTP client, and what are the default values of its `timeout` and `httpVersion` fields?",
		Shape:    `{"recordName": "<name>", "timeoutDefault": "<value>", "httpVersionDefault": "<value>"}`,
		Fields: []Field{
			{Key: "recordName", Kind: kindName, Want: one("ClientConfiguration")},
			{Key: "timeoutDefault", Kind: kindValue, Want: one("30")},
			{Key: "httpVersionDefault", Kind: kindValue, Want: one("HTTP_2_0", "2.0")},
		},
		FirstCommands: map[string][][]string{
			"A": {{"ballerina/http:2.16.6", "type", "ClientConfiguration"}, {"ballerina/http:2.16.6", "type"}},
			"B": {{"type", "ClientConfiguration", "-p", "ballerina/http:2.16.6"}, {"type", "-p", "ballerina/http:2.16.6"}},
		},
	},
	{
		ID: "D07", Category: "Class",
		Question: "In ballerina/http version 2.16.6, the `Response` object has an operation that reads the body as JSON. Give its name and return type.",
		Shape:    `{"operation": "<name>", "returnType": "<type>"}`,
		Fields: []Field{
			{Key: "operation", Kind: kindName, Want: one("getJsonPayload")},
			{Key: "returnType", Kind: kindType, Want: one("json|ClientError")},
		},
		FirstCommands: map[string][][]string{
			"A": {{"ballerina/http:2.16.6", "class", "Response", "getJsonPayload"}, {"ballerina/http:2.16.6", "class", "Response"}},
			"B": {{"method", "getJsonPayload", "-p", "ballerina/http:2.16.6", "--class", "Response"}, {"method", "-p", "ballerina/http:2.16.6", "--class", "Response"}},
		},
	},
	{
		ID: "D08", Category: "Resource path + wrong guess",
		Question: "In ballerinax/github version 6.1.0, a teammate says the client has a `createIssue` operation for creating an issue in a repository. Find how the client really does it: the HTTP accessor, the path (with path parameters written as `:name`), the type of the payload, that payload's required field(s), and the return type.",
		Shape:    `{"accessor": "<get|post|...>", "path": "<path>", "payloadType": "<type>", "requiredFields": ["<field>", "..."], "returnType": "<type>"}`,
		Fields: []Field{
			{Key: "accessor", Kind: kindName, Want: one("post")},
			{Key: "path", Kind: kindPath, Want: one("repos/:owner/:repo/issues")},
			{Key: "payloadType", Kind: kindType, Want: one("RepoIssuesBody")},
			{Key: "requiredFields", Kind: kindNames, Want: one("title")},
			{Key: "returnType", Kind: kindType, Want: one("Issue|error")},
		},
		FirstCommands: map[string][][]string{
			"A": {{"ballerinax/github:6.1.0", "search", "issues"}},
			"B": {{"search", "issues", "-p", "ballerinax/github:6.1.0"}},
		},
	},
	{
		ID: "D09", Category: "Escaped path segment",
		Question: "In ballerinax/github version 6.1.0, how do you list public gists with the client? Give the HTTP accessor, the path, and the return type.",
		Shape:    `{"accessor": "<get|post|...>", "path": "<path>", "returnType": "<type>"}`,
		Fields: []Field{
			{Key: "accessor", Kind: kindName, Want: one("get")},
			{Key: "path", Kind: kindPath, Want: one("gists/public")},
			{Key: "returnType", Kind: kindType, Want: one("BaseGist[]|error?")},
		},
		FirstCommands: map[string][][]string{
			"A": {{"ballerinax/github:6.1.0", "search", "gists"}},
			"B": {{"search", "gists", "-p", "ballerinax/github:6.1.0"}},
		},
	},
	{
		ID: "D10", Category: "Wrong guess",
		Question: "In ballerinax/twilio version 5.0.2, a teammate's code `twilio->sendMessage(...)` doesn't compile. Which client operation sends an SMS, what type is its payload, which field(s) of that payload are required, and what does it return?",
		Shape:    `{"operation": "<name>", "payloadType": "<type>", "requiredFields": ["<field>", "..."], "returnType": "<type>"}`,
		Fields: []Field{
			{Key: "operation", Kind: kindName, Want: one("createMessage")},
			{Key: "payloadType", Kind: kindType, Want: one("CreateMessageRequest")},
			{Key: "requiredFields", Kind: kindNames, Want: one("To")},
			{Key: "returnType", Kind: kindType, Want: one("Message|error")},
		},
		FirstCommands: map[string][][]string{
			"A": {{"ballerinax/twilio:5.0.2", "search", "message"}},
			"B": {{"search", "message", "-p", "ballerinax/twilio:5.0.2"}},
		},
	},
	{
		ID: "D11", Category: "Large surface / paging",
		Question: "In ballerinax/twilio version 5.0.2, how many remote operations (called with `->`) does the client have in total? Which one fetches a single call by its SID, and what does it return?",
		Shape:    `{"operationCount": 0, "operation": "<name>", "returnType": "<type>"}`,
		Fields: []Field{
			{Key: "operationCount", Kind: kindInt, Want: one("199")},
			{Key: "operation", Kind: kindName, Want: one("fetchCall")},
			{Key: "returnType", Kind: kindType, Want: one("Call|error")},
		},
		FirstCommands: map[string][][]string{
			"A": {{"ballerinax/twilio:5.0.2", "client", "Client"}},
			"B": {{"method", "-p", "ballerinax/twilio:5.0.2", "--client", "Client"}},
		},
	},
	{
		ID: "D12", Category: "Service + listener defaults",
		Question: "In ballerina/ftp version 2.22.0, a service watches a remote directory. Which handler is invoked when a file is deleted, and what are its parameter types in order? What are the listener's default polling interval and default port?",
		Shape:    `{"handler": "<name>", "parameterTypes": ["<type>", "..."], "pollingIntervalDefault": "<value>", "portDefault": "<value>"}`,
		Fields: []Field{
			{Key: "handler", Kind: kindName, Want: one("onFileDelete")},
			{Key: "parameterTypes", Kind: kindTypes, Want: one("string", "Caller")},
			{Key: "pollingIntervalDefault", Kind: kindValue, Want: one("60")},
			{Key: "portDefault", Kind: kindValue, Want: one("21")},
		},
		FirstCommands: map[string][][]string{
			"A": {{"ballerina/ftp:2.22.0", "service", "Service"}},
			"B": {{"service", "Service", "-p", "ballerina/ftp:2.22.0"}},
		},
	},
	{
		ID: "D13", Category: "Readme example",
		Question: "The documentation of ballerinax/postgresql version 1.19.1 has an example of inserting many rows in one call. Which client operation does it use, what is the type of the batch it passes, and what does the call return in that example?",
		Shape:    `{"operation": "<name>", "batchType": "<type>", "returnType": "<type>"}`,
		Fields: []Field{
			{Key: "operation", Kind: kindName, Want: one("batchExecute")},
			{Key: "batchType", Kind: kindType, Want: one("ParameterizedQuery[]")},
			{Key: "returnType", Kind: kindType, Want: one("ExecutionResult[]", "ExecutionResult[]|Error")},
		},
		FirstCommands: map[string][][]string{
			"A": {{"ballerinax/postgresql:1.19.1", "readme"}},
			"B": {{"readme", "-p", "ballerinax/postgresql:1.19.1"}},
		},
	},
	{
		ID: "D14", Category: "Constructor defaults",
		Question: "In ballerinax/postgresql version 1.19.1, when you create the database client without arguments, which host, username and port does it use by default?",
		Shape:    `{"host": "<value>", "username": "<value>", "port": "<value>"}`,
		Fields: []Field{
			{Key: "host", Kind: kindValue, Want: one("localhost")},
			{Key: "username", Kind: kindValue, Want: one("postgres")},
			{Key: "port", Kind: kindValue, Want: one("5432")},
		},
		FirstCommands: map[string][][]string{
			"A": {{"ballerinax/postgresql:1.19.1", "client", "Client", "init"}},
			"B": {{"method", "init", "-p", "ballerinax/postgresql:1.19.1", "--client", "Client"}},
		},
	},
	{
		ID: "D15", Category: "Generic return",
		Question: "In ballerina/sql version 1.19.0, which client operation returns exactly one row? Give its name, the type of its first parameter, and its return type.",
		Shape:    `{"operation": "<name>", "firstParameterType": "<type>", "returnType": "<type>"}`,
		Fields: []Field{
			{Key: "operation", Kind: kindName, Want: one("queryRow")},
			{Key: "firstParameterType", Kind: kindType, Want: one("ParameterizedQuery")},
			{Key: "returnType", Kind: kindType, Want: one("returnType|Error")},
		},
		FirstCommands: map[string][][]string{
			"A": {{"ballerina/sql:1.19.0", "client", "queryRow"}, {"ballerina/sql:1.19.0", "client", "Client"}},
			"B": {{"method", "queryRow", "-p", "ballerina/sql:1.19.0"}, {"method", "-p", "ballerina/sql:1.19.0", "--client", "Client"}},
		},
	},
	{
		ID: "D16", Category: "Find via search",
		Question: "Which library on Ballerina Central is the Amazon S3 connector (give it as org/name)? Which of its client operations uploads in-memory content as an object? Give the operation's parameter types in order and its return type.",
		Shape:    `{"library": "<org>/<name>", "operation": "<name>", "parameterTypes": ["<type>", "..."], "returnType": "<type>"}`,
		Fields: []Field{
			{Key: "library", Kind: kindLibrary, Want: one("ballerinax/aws.s3")},
			{Key: "operation", Kind: kindName, Want: one("putObject")},
			{Key: "parameterTypes", Kind: kindTypes, Want: one("string", "string", "UploadContent", "PutObjectConfig")},
			{Key: "returnType", Kind: kindType, Want: one("Error?")},
		},
		FirstCommands: map[string][][]string{
			"A": {{"search", "s3"}, {"search", "aws", "s3"}, {"search", "amazon", "s3"}},
			"B": {{"search", "s3"}, {"search", "aws", "s3"}, {"search", "amazon", "s3"}},
		},
	},
	{
		ID: "D17", Category: "Submodule",
		Question: "ballerina/graphql version 1.19.0 ships a separate module for data loaders. What is that module's import path, what is the type of the parameter of the default data loader's constructor, and what does the data loader's `get` operation return?",
		Shape:    `{"importPath": "<org>/<module>", "constructorParameterType": "<type>", "getReturnType": "<type>"}`,
		Fields: []Field{
			{Key: "importPath", Kind: kindLibrary, Want: one("ballerina/graphql.dataloader")},
			{Key: "constructorParameterType", Kind: kindType, Want: one("BatchLoadFunction")},
			{Key: "getReturnType", Kind: kindType, Want: one("'type|error")},
		},
		FirstCommands: map[string][][]string{
			"A": {{"ballerina/graphql:1.19.0"}},
			"B": {{"package", "ballerina/graphql:1.19.0"}},
		},
	},
	{
		ID: "D18", Category: "Module-level call",
		Question: "In ballerina/io version 1.8.2, what do you call to read a JSON file into a value? Give its name, its parameters in order (name and type) and its return type.",
		Shape:    `{"operation": "<name>", "parameters": [{"name": "<name>", "type": "<type>"}], "returnType": "<type>"}`,
		Fields: []Field{
			{Key: "operation", Kind: kindName, Want: one("fileReadJson")},
			{Key: "parameters", Kind: kindParams, Params: []Param{{Name: "path", Type: "string"}}},
			{Key: "returnType", Kind: kindType, Want: one("json|Error")},
		},
		FirstCommands: map[string][][]string{
			"A": {{"ballerina/io:1.8.2", "funcs", "fileReadJson"}},
			"B": {{"function", "fileReadJson", "-p", "ballerina/io:1.8.2"}},
		},
	},
	{
		ID: "D19", Category: "Find by purpose + nested record",
		Question: "Which library on Ballerina Central sends email through Gmail (give it as org/name)? With its client, how do you send an email message: give the HTTP accessor, the path (path parameters written as `:name`), the payload's type, the type that describes one attachment in that payload, that attachment type's required fields, and the return type.",
		Shape:    `{"library": "<org>/<name>", "accessor": "<get|post|...>", "path": "<path>", "payloadType": "<type>", "attachmentType": "<type>", "attachmentRequiredFields": ["<field>", "..."], "returnType": "<type>"}`,
		Fields: []Field{
			{Key: "library", Kind: kindLibrary, Want: one("ballerinax/googleapis.gmail")},
			{Key: "accessor", Kind: kindName, Want: one("post")},
			{Key: "path", Kind: kindPath, Want: one("users/:userId/messages/send")},
			{Key: "payloadType", Kind: kindType, Want: one("MessageRequest")},
			{Key: "attachmentType", Kind: kindType, Want: one("AttachmentFile")},
			{Key: "attachmentRequiredFields", Kind: kindNames, Want: one("mimeType", "name", "path")},
			{Key: "returnType", Kind: kindType, Want: one("Message|error")},
		},
		FirstCommands: map[string][][]string{
			"A": {{"search", "gmail"}, {"search", "gmail", "send"}, {"search", "gmail", "email"}, {"search", "email"}},
			"B": {{"search", "gmail"}, {"search", "gmail", "send"}, {"search", "gmail", "email"}, {"search", "email"}},
		},
	},
	{
		ID: "D20", Category: "Listener + service + second library",
		Question: "A Ballerina integration should copy every message posted in a public Slack channel into another Slack channel. Which Central library provides the listener that receives Slack events, and which one posts messages to Slack (org/name each)? Which service type and which handler receive a message posted in a public channel, and what is that handler's parameter type? Which field must the listener's configuration record have? To post the copy, give the HTTP accessor, the path, and the required field(s) of its payload.",
		Shape:    `{"listenerLibrary": "<org>/<name>", "senderLibrary": "<org>/<name>", "serviceType": "<name>", "handler": "<name>", "handlerParameterType": "<type>", "listenerConfigRequiredFields": ["<field>", "..."], "postAccessor": "<get|post|...>", "postPath": "<path>", "postRequiredFields": ["<field>", "..."]}`,
		Fields: []Field{
			{Key: "listenerLibrary", Kind: kindLibrary, Want: one("ballerinax/trigger.slack")},
			{Key: "senderLibrary", Kind: kindLibrary, Want: one("ballerinax/slack")},
			{Key: "serviceType", Kind: kindType, Want: one("MessageService")},
			{Key: "handler", Kind: kindName, Want: one("onMessageChannels")},
			{Key: "handlerParameterType", Kind: kindType, Want: one("GenericEventWrapper")},
			{Key: "listenerConfigRequiredFields", Kind: kindNames, Want: one("verificationToken")},
			{Key: "postAccessor", Kind: kindName, Want: one("post")},
			{Key: "postPath", Kind: kindPath, Want: one("chat.postMessage")},
			{Key: "postRequiredFields", Kind: kindNames, Want: one("channel")},
		},
		FirstCommands: map[string][][]string{
			"A": {{"search", "slack"}, {"search", "slack", "listener"}, {"search", "slack", "events"}, {"search", "slack", "message"}},
			"B": {{"search", "slack"}, {"search", "slack", "listener"}, {"search", "slack", "events"}, {"search", "slack", "message"}},
		},
	},
	{
		ID: "D21", Category: "Two ambiguous names",
		Question: "In ballerinax/kafka version 4.6.6, two operations exist on more than one client. For the consumer's `close`, give its parameters (name, type and default value) and its return type. For the producer's `getTopicPartitions`, give its parameters (name and type) and its return type.",
		Shape:    `{"consumerCloseParameters": [{"name": "<name>", "type": "<type>", "default": "<default value, or omit the key>"}], "consumerCloseReturnType": "<type>", "producerGetTopicPartitionsParameters": [{"name": "<name>", "type": "<type>"}], "producerGetTopicPartitionsReturnType": "<type>"}`,
		Fields: []Field{
			{Key: "consumerCloseParameters", Kind: kindParams, Params: []Param{{Name: "duration", Type: "decimal", Default: "-1"}}},
			{Key: "consumerCloseReturnType", Kind: kindType, Want: one("Error?")},
			{Key: "producerGetTopicPartitionsParameters", Kind: kindParams, Params: []Param{{Name: "topic", Type: "string"}}},
			{Key: "producerGetTopicPartitionsReturnType", Kind: kindType, Want: one("TopicPartition[]|Error")},
		},
		FirstCommands: map[string][][]string{
			"A": {{"ballerinax/kafka:4.6.6", "client", "Consumer", "close"}, {"ballerinax/kafka:4.6.6", "client", "Producer", "getTopicPartitions"}},
			"B": {{"method", "close", "-p", "ballerinax/kafka:4.6.6", "--client", "Consumer"}, {"method", "getTopicPartitions", "-p", "ballerinax/kafka:4.6.6", "--client", "Producer"}},
		},
	},
	{
		ID: "D22", Category: "Submodule without a hint",
		Question: "ballerina/ai version 1.16.0 ships a separate module for tracing AI calls. What is that module's import path? Which call in that module creates a span for a chat request, what are its parameters (name and type) and its return type, and how many operations does the returned span object offer?",
		Shape:    `{"importPath": "<org>/<module>", "operation": "<name>", "parameters": [{"name": "<name>", "type": "<type>"}], "returnType": "<type>", "spanOperationCount": 0}`,
		Fields: []Field{
			{Key: "importPath", Kind: kindLibrary, Want: one("ballerina/ai.observe")},
			{Key: "operation", Kind: kindName, Want: one("createChatSpan")},
			{Key: "parameters", Kind: kindParams, Params: []Param{{Name: "llmModel", Type: "string"}}},
			{Key: "returnType", Kind: kindType, Want: one("ChatSpan")},
			{Key: "spanOperationCount", Kind: kindInt, Want: one("14")},
		},
		FirstCommands: map[string][][]string{
			"A": {{"ballerina/ai:1.16.0"}},
			"B": {{"package", "ballerina/ai:1.16.0"}},
		},
	},
	{
		ID: "D23", Category: "Two path parameters + payload",
		Question: "In ballerinax/github version 6.1.0, how do you open a pull request with the client? Give the HTTP accessor, the path (path parameters written as `:name`), the payload type, its required fields, and the return type.",
		Shape:    `{"accessor": "<get|post|...>", "path": "<path>", "payloadType": "<type>", "requiredFields": ["<field>", "..."], "returnType": "<type>"}`,
		Fields: []Field{
			{Key: "accessor", Kind: kindName, Want: one("post")},
			{Key: "path", Kind: kindPath, Want: one("repos/:owner/:repo/pulls")},
			{Key: "payloadType", Kind: kindType, Want: one("RepoPullsBody")},
			{Key: "requiredFields", Kind: kindNames, Want: one("head", "base")},
			{Key: "returnType", Kind: kindType, Want: one("PullRequest|error")},
		},
		FirstCommands: map[string][][]string{
			"A": {{"ballerinax/github:6.1.0", "search", "pulls"}, {"ballerinax/github:6.1.0", "search", "pull"}, {"ballerinax/github:6.1.0", "client", "Client", "repos/:owner/:repo/pulls", "post"}},
			"B": {{"search", "pulls", "-p", "ballerinax/github:6.1.0"}, {"search", "pull", "-p", "ballerinax/github:6.1.0"}, {"resource", "repos/:owner/:repo/pulls", "post", "-p", "ballerinax/github:6.1.0", "--client", "Client"}},
		},
	},
	{
		ID: "D24", Category: "Wrong in two ways (call chain)",
		Question: "A colleague's code `mongo->find(\"mydb\", \"users\", {})`, where `mongo` is a ballerinax/mongodb version 5.2.4 client, does not compile. Starting from that client, give the chain of calls (operation names, in order) that reaches the operation that finds documents, the name of the client type that declares that find operation, the type of its first parameter, and its return type.",
		Shape:    `{"callChain": ["<operation>", "..."], "findDeclaredBy": "<type name>", "firstParameterType": "<type>", "returnType": "<type>"}`,
		Fields: []Field{
			{Key: "callChain", Kind: kindChain, Want: one("getDatabase", "getCollection", "find")},
			{Key: "findDeclaredBy", Kind: kindType, Want: one("Collection")},
			{Key: "firstParameterType", Kind: kindType, Want: one("map<json>")},
			{Key: "returnType", Kind: kindType, Want: one("stream<targetType, error?>|Error")},
		},
		FirstCommands: map[string][][]string{
			"A": {{"ballerinax/mongodb:5.2.4", "client", "Client"}, {"ballerinax/mongodb:5.2.4", "client"}, {"ballerinax/mongodb:5.2.4"}},
			"B": {{"method", "-p", "ballerinax/mongodb:5.2.4", "--client", "Client"}, {"client", "-p", "ballerinax/mongodb:5.2.4"}, {"package", "ballerinax/mongodb:5.2.4"}},
		},
	},
}

// HardIDs are the multi-step tasks added after the pilot put D01-D18 at ceiling.
var HardIDs = []string{"D19", "D20", "D21", "D22", "D23", "D24"}

// PilotIDs is the 6-task pilot subset, one per behaviour.
var PilotIDs = []string{"D02", "D03", "D08", "D10", "D13", "D17"}

// ScenarioName is the scenario name a task is registered under.
func ScenarioName(id string) string { return "dq-" + id }
