# LLMTest

It's a tool to help TiDB developers to generate test cases through LLM for any features, and run the test cases in TiDB.

## Generate

The `LLMTest` uses different "prompt generator" to generate the tests for different features. For example, the `generator/expression.go` includes the expression prompt generator to generate the test cases for expression module.

To generate test cases with a specific prompt generator, you can use the following command:

```bash
./llmtest generate --openai_base_url https://openai.base.url --openai_model deepseek/deepseek-r1 --openai_token XXXXX --parallel 20 --prompt_generator expression --test_count 10
```

Replace the `prompt_generator` with the specific prompt generator you want to use.

The meaning of `--test_count` depends on the generator:

- For `expression`, `dml`, and `misc`, it is the target number of generated cases for each group.
- For one-shot generators such as `issuepattern`, `bugseed`, and `oracle`, each group is generated with a single prompt, so `--test_count` does not control the final number of returned items.

For the `issuepattern` generator, you must also provide the input issue file:

```bash
./llmtest generate --openai_base_url https://openai.base.url --openai_model deepseek/deepseek-r1 --openai_token XXXXX --parallel 20 --prompt_generator issuepattern --test_count 1 --issue_seed_file <issue_file>.json
```

The `--issue_seed_file` flag is only used by the `issuepattern` generator.

## Issuepattern Input

The input file for `issuepattern` must be a JSON array. Each element should be an object with the following fields:

- `title`: short bug summary.
- `description`: bug description in natural language.
- `steps`: array of SQL or reproduction steps.
- `links`: array of related links. If a MySQL bug link contains `id=<number>`, `issuepattern` uses that number to derive the bug ID.
- `source`: optional source label such as `mysql-bugs`.
- `version`: optional upstream version information.

Unknown fields are ignored.

Example:

```json
[
  {
    "source": "mysql-bugs",
    "title": "Server crash with CTE and UNION ALL",
    "description": "The server crashes when a recursive CTE involves a UNION ALL operation.",
    "steps": [
      "CREATE TABLE tree (id INT, path VARCHAR(100));",
      "INSERT INTO tree VALUES (1, 'root');",
      "WITH RECURSIVE cte (id, path) AS (SELECT id, path FROM tree UNION ALL SELECT id + 1, path FROM cte WHERE id < 5) SELECT * FROM cte;"
    ],
    "version": "8.0.23",
    "links": [
      "https://bugs.mysql.com/bug.php?id=12345"
    ]
  }
]
```

## Add a new prompt generator

To add a new prompt generator, you need to implement the `PromptGenerator` interface in the `generator/prompt.go` file. The `PromptGenerator` interface includes the following methods:

```go
// PromptGenerator is the interface for prompt generator.
type PromptGenerator[T any] interface {
	Name() string
	Kind() GeneratorKind
	OneShotPerGroup() bool
	Groups() []string

	GeneratePrompt(group string, count int, existItems []T) []openai.ChatCompletionMessageParamUnion
	Unmarshal(response string) []T
}
```

The `Name` method returns the name of the prompt generator. The `Groups` method returns the sub-classes of the prompt generator. `OneShotPerGroup()` reports whether a generator should run once per group or keep generating until the target `test_count` is reached. The `GeneratePrompt` method generates the prompt for the test cases. The `Unmarshal` method unmarshals the response from the OpenAI API to the generated items.

The `Groups()` method is used to classify the test cases. For example, the `expression` prompt generator has the following groups:

```go
[]string{"+", "-" ....}
```

## Verify

To verify the generated test cases, you can use the following command:

```bash
./llmtest verify --mysql_dsn "root:123456@tcp(127.0.0.1:3306)/test" --tidb_dsn "root@tcp(127.0.0.1:4000)/test" --prompt_generator expression
```

It'll execute the generated test cases in the TiDB cluster and MySQL to verify the results.
