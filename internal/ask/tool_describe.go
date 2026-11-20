package ask

import "encoding/json"

// DescribeToolCall says what a recorded call of a built-in tool asked for and
// what it found, in the words of the Slack progress line: asked from the
// call's input, found from its output. A tool this package does not build,
// or an output that is missing or not the tool's shape, leaves found (and
// asked) empty, for the caller to describe generically.
func DescribeToolCall(name string, input, output json.RawMessage) (asked, found string) {
	var status func(json.RawMessage) StatusLine
	var result func(json.RawMessage) string
	switch name {
	case "retrieve":
		status, result = retrieveStatus, retrieveStatusOutput
	case "list":
		status, result = listStatus, listStatusOutput
	case "expand":
		status = expandStatus
		result = func(output json.RawMessage) string { return expandStatusOutput(output, nil) }
	case "web_search":
		status, result = webSearchStatus, webSearchStatusOutput
	case "web_fetch":
		status, result = webFetchStatus, webFetchStatusOutput
	case "code_search":
		status, result = codeSearchStatus, codeSearchStatusOutput
	case "code_read":
		status, result = codeReadStatus, codeReadStatusOutput
	case InvestigateToolName:
		presentation := investigatePresentation()
		status = presentation.Status
	default:
		return "", ""
	}
	asked = status(input).Detail
	if result != nil && len(output) > 0 {
		found = result(output)
	}
	return asked, found
}
