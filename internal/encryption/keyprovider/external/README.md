# External key provider


> [!WARNING]
> This file is not an end-user documentation, it is intended for developers. Please follow the user documentation on the OpenTofu website unless you want to work on the encryption code.

This directory contains the `external` key provider. You can configure it like this:

```hcl
terraform {
  encryption {
    key_provider "external" "foo" {
      command = ["/path/to/binary", "arg1", "arg2"]
    }
  }
}
```

The external key provider must implement the following protocol:

1. On start, the provider must emit the header line matching [the header schema](protocol/header.schema.json) on the standard output.
2. OpenTofu supplies `null` or the input metadata matching [the input schema](protocol/input.schema.json) on the standard input.
3. The provider must emit the key material matching [the output schema](protocol/output.schema.json) on the standard output.