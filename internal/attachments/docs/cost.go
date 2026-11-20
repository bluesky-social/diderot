package docs

// Checked before starting each page; the page that crosses it is retained.
const maxDocumentCostUSD = 10.0

type modelRates struct {
	inputUSDPerMTok  float64
	outputUSDPerMTok float64
}

// Estimates: list prices as of November 2026.
//
// extractionModelRates are the prices of the models priced below
// unlistedModelRates, by model ID.
var extractionModelRates = map[string]modelRates{
	"gpt-5.4-mini:api": {inputUSDPerMTok: 0.75, outputUSDPerMTok: 4.5},
	"claude-sonnet-5":  {inputUSDPerMTok: 2, outputUSDPerMTok: 10},
}

// unlistedModelRates price every other model, so that a model the table
// lacks never disables the fuse.
var unlistedModelRates = modelRates{inputUSDPerMTok: 2.5, outputUSDPerMTok: 15}

func extractionCostUSD(modelID string, inputTokens, outputTokens int) float64 {
	rates, ok := extractionModelRates[modelID]
	if !ok {
		rates = unlistedModelRates
	}
	return float64(inputTokens)*rates.inputUSDPerMTok/1e6 +
		float64(outputTokens)*rates.outputUSDPerMTok/1e6
}
