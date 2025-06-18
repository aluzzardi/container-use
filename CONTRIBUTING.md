# Contributing to Container Use

Thank you for your interest in contributing to Container Use! This document outlines the necessary steps and standards to follow when contributing.

## Development Setup

Follow these steps to set up your development environment:

1. **Install Go**: Ensure you have Go version 1.21 or higher installed.
2. **Clone the Repository**:

   ```bash
   git clone git@github.com:dagger/container-use.git
   ```
3. **Install Dependencies**:

   ```bash
   go mod download
   ```
4. **Container Runtime**: Ensure you have a compatible container runtime installed (e.g., Docker).

## Building

To build the `cu` binary without installing it to your `$PATH`, you can use either Dagger or Go directly:

### Using Go

```bash
go build -o cu ./cmd/cu
```

### Using Dagger

```bash
dagger call build --platform=current export --path ./cu
```

## Testing

Container Use includes both unit and integration tests to maintain high code quality and functionality.

### Running Tests

* **Run All Tests**:

  ```bash
  go test ./...
  ```

* **Run Unit Tests Only** (fast, no containers):

  ```bash
  go test -short ./...
  ```

* **Run Integration Tests Only**:

  ```bash
  go test -count=1 -v ./environment
  ```

### Test Structure

Tests are structured as follows:

* **`environment_test.go`**: Contains unit tests for package logic.
* **`integration_test.go`**: Covers integration scenarios to verify environment stability and state transitions.
* **`test_helpers.go`**: Provides shared utility functions for writing tests.

### Writing Tests

When contributing new features or fixing issues, adhere to these guidelines:

1. Write clear **unit tests** for the core logic.
2. Create comprehensive **integration tests** for validating end-to-end functionality.
3. Utilize provided **test helpers** for common tasks to maintain consistency.
4. Follow existing test patterns and naming conventions.

## Code Style

Maintain code consistency and readability by:

* Following standard Go coding conventions.
* Formatting code using `go fmt` before committing.
* Ensuring all tests pass locally before submitting your pull request.

## Submitting Changes

Submit contributions using these steps:

1. Fork the Container Use repository.
2. Create a descriptive feature branch from the main branch.
3. Commit your changes, including relevant tests.
4. Open a pull request with a clear and descriptive explanation of your changes.
