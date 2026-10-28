# Documentation Rules

- Do not modify the `README.md` file in the project root directory unless explicitly requested.

# Engineering Principles

## Code Quality

- Write clear, readable, and maintainable code.
- Prefer simple, consistent, and well-structured solutions over unnecessary complexity.
- Keep responsibilities focused and code intent explicit.
- Avoid unnecessary abstractions, indirection, and hidden behavior.

## Architecture and Dependencies

- Favor high cohesion and low coupling, while recognizing that meaningful collaboration requires dependencies.
- Keep module boundaries clear and dependencies intentional, explicit, and manageable.
- Prefer well-defined interfaces and appropriate dependency direction.
- Consider system-wide implications and address problems at the appropriate architectural level.
- Avoid unnecessary complexity, special cases, and workarounds that compromise maintainability.

# Bug Fixing Principles

- Identify the root cause before implementing a fix.
- Determine whether the issue is local or reflects a broader design problem.
- Address the underlying cause rather than merely treating symptoms.
- Avoid temporary workarounds unless justified by clear constraints.

# Compatibility Principles

- Backward compatibility is not required by default.
- Prioritize correctness, maintainability, and architectural consistency over preserving legacy behavior.
- Breaking changes are acceptable when they improve the overall system design or resolve fundamental issues.

# Before Making Changes

- Understand the relevant code, existing conventions, and intended behavior before making changes.
- Keep changes focused on the requested task and avoid unrelated modifications.
- Match the depth of analysis to the complexity and impact of the change:
  - **Simple changes:** Inspect the relevant code and implement directly without unnecessary analysis.
  - **Feature development:** Identify affected components, responsibilities, dependencies, and potential architectural implications.
  - **Bug fixes:** Investigate the root cause and determine whether a local fix or broader design change is appropriate.
  - **Significant architectural changes:** Evaluate alternatives, trade-offs, and long-term maintainability before implementation.
- Prefer the simplest solution that fully addresses the requirements without compromising correctness or maintainability.

# Git Commit Conventions

When creating Git commits in this repository:

- Write commit messages in English.
- Use an imperative, sentence-case subject.
- Describe the outcome of the change, not the development activity.
- Do not use Conventional Commit prefixes such as `feat:`, `fix:`, or `chore:`.
- Keep the subject at 75 characters or fewer and do not end it with a period.
- Keep each commit focused on one conceptual change.
- Use backticks around code identifiers when helpful.
- For non-trivial commits, structure the body with:
  - `## Why` to explain the problem or motivation.
  - `## What changed` to summarize the implementation.
  - `## Testing` to list actual tests and validation performed.
- Omit `## Why` when the motivation is obvious.
- If tests were not run, state that explicitly and explain why.
- Only create a Git commit when the user explicitly asks for one.

Example subject:

`Expose thread originators through the app-server API`