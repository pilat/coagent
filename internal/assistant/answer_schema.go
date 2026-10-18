package assistant

// AnswerSchema preserves field order because it determines strict-decoder output order.
const AnswerSchema = `{
  "type": "object",
  "properties": {
    "say": {"type": ["string", "null"]},
    "do": {
      "type": "array",
      "items": {
        "anyOf": [
          {
            "type": "object",
            "properties": {
              "open": {
                "type": "object",
                "properties": {
                  "project": {"type": "string"},
                  "about": {"type": "string"},
                  "text": {"type": "string"}
                },
                "required": ["project", "about", "text"],
                "additionalProperties": false
              }
            },
            "required": ["open"],
            "additionalProperties": false
          },
          {
            "type": "object",
            "properties": {
              "send": {
                "type": "object",
                "properties": {
                  "session": {"type": "integer"},
                  "text": {"type": "string"}
                },
                "required": ["session", "text"],
                "additionalProperties": false
              }
            },
            "required": ["send"],
            "additionalProperties": false
          },
          {
            "type": "object",
            "properties": {
              "read": {
                "type": "object",
                "properties": {
                  "session": {"type": "integer"},
                  "lines": {"type": "integer"}
                },
                "required": ["session", "lines"],
                "additionalProperties": false
              }
            },
            "required": ["read"],
            "additionalProperties": false
          },
          {
            "type": "object",
            "properties": {
              "stop": {
                "type": "object",
                "properties": {"session": {"type": "integer"}},
                "required": ["session"],
                "additionalProperties": false
              }
            },
            "required": ["stop"],
            "additionalProperties": false
          }
        ]
      }
    },
    "focus": {"type": ["string", "null"]},
    "notes": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "key": {"type": "string"},
          "text": {"type": ["string", "null"]}
        },
        "required": ["key", "text"],
        "additionalProperties": false
      }
    },
    "forget": {"type": "array", "items": {"type": "array", "items": {"type": "integer"}}}
  },
  "required": ["say", "do", "focus", "notes", "forget"],
  "additionalProperties": false
}`
