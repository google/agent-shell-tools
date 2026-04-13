// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//! Wire protocol for pktexec.
//!
//! Messages are serialized with postcard over a SOCK_SEQPACKET Unix socket.
//! Each sendmsg/recvmsg is one complete message; no framing is needed.

use serde::{Deserialize, Serialize};

/// Messages sent from the client to the server.
#[derive(Debug, PartialEq, Serialize, Deserialize)]
pub enum ClientMessage {
    /// First message on a connection. The three stdio file descriptors are
    /// attached via SCM_RIGHTS ancillary data alongside this message.
    ExecRequest {
        argv: Vec<String>,
        working_dir: String,
    },
    /// Forwarded signal (SIGTERM, SIGINT, SIGQUIT). May be sent zero or more
    /// times while the command is running.
    TermSignal { signo: i32 },
}

/// Messages sent from the server to the client.
#[derive(Debug, PartialEq, Serialize, Deserialize)]
pub enum ServerMessage {
    /// Sent exactly once in response to ExecRequest.
    FilterResult { allowed: bool, reason: String },
    /// Sent exactly once after the command terminates (only if allowed).
    Exit { code: i32 },
}

/// Serialize a message to bytes.
pub fn encode<T: Serialize>(msg: &T) -> anyhow::Result<Vec<u8>> {
    postcard::to_allocvec(msg).map_err(Into::into)
}

/// Deserialize a message from bytes.
pub fn decode<T: for<'a> Deserialize<'a>>(buf: &[u8]) -> anyhow::Result<T> {
    postcard::from_bytes(buf).map_err(Into::into)
}

#[cfg(test)]
mod tests {
    use super::*;

    use googletest::prelude::*;

    #[googletest::test]
    fn roundtrip_exec_request() {
        let msg = ClientMessage::ExecRequest {
            argv: vec!["gh".into(), "run".into(), "view".into(), "123".into()],
            working_dir: "/workspace".into(),
        };
        let bytes = encode(&msg).unwrap();
        let decoded: ClientMessage = decode(&bytes).unwrap();
        expect_that!(decoded, eq(&msg));
    }

    #[googletest::test]
    fn roundtrip_exec_request_empty_working_dir() {
        let msg = ClientMessage::ExecRequest {
            argv: vec!["echo".into()],
            working_dir: String::new(),
        };
        let bytes = encode(&msg).unwrap();
        let decoded: ClientMessage = decode(&bytes).unwrap();
        expect_that!(decoded, eq(&msg));
    }

    #[googletest::test]
    fn roundtrip_term_signal() {
        let msg = ClientMessage::TermSignal { signo: 15 };
        let bytes = encode(&msg).unwrap();
        let decoded: ClientMessage = decode(&bytes).unwrap();
        expect_that!(decoded, eq(&msg));
    }

    #[googletest::test]
    fn roundtrip_filter_result_allowed() {
        let msg = ServerMessage::FilterResult {
            allowed: true,
            reason: String::new(),
        };
        let bytes = encode(&msg).unwrap();
        let decoded: ServerMessage = decode(&bytes).unwrap();
        expect_that!(decoded, eq(&msg));
    }

    #[googletest::test]
    fn roundtrip_filter_result_denied() {
        let msg = ServerMessage::FilterResult {
            allowed: false,
            reason: "command not permitted".into(),
        };
        let bytes = encode(&msg).unwrap();
        let decoded: ServerMessage = decode(&bytes).unwrap();
        expect_that!(decoded, eq(&msg));
    }

    #[googletest::test]
    fn roundtrip_exit() {
        let msg = ServerMessage::Exit { code: 42 };
        let bytes = encode(&msg).unwrap();
        let decoded: ServerMessage = decode(&bytes).unwrap();
        expect_that!(decoded, eq(&msg));
    }

    #[googletest::test]
    fn decode_truncated_returns_error() {
        let bytes = encode(&ClientMessage::TermSignal { signo: 15 }).unwrap();
        let result = decode::<ClientMessage>(&bytes[..bytes.len() - 1]);
        expect_that!(result.is_err(), eq(true));
    }

    #[googletest::test]
    fn decode_empty_returns_error() {
        let result = decode::<ClientMessage>(&[]);
        expect_that!(result.is_err(), eq(true));
    }
}
