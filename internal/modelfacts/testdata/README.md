# Test fixtures

`models.dev.json` and `litellm.json` are small extracts of two published
lists, cut down to the providers the matching rules name and the models the
tests ask about, with only the fields Diderot reads:

- [models.dev](https://models.dev)'s `api.json`, MIT License, Copyright (c)
  2025 models.dev.
- [LiteLLM](https://github.com/BerriAI/litellm)'s
  `model_prices_and_context_window.json`, MIT License, Copyright (c) 2023
  Berri AI.

They are a snapshot for tests: nothing reads them at run time, and their
prices and dates are not kept current.
