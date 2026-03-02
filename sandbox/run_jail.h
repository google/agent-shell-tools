/*
 * Copyright 2026 Google LLC
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

#ifndef RUN_JAIL_H_
#define RUN_JAIL_H_

#include <stddef.h>

#ifdef __cplusplus
extern "C" {
#endif

/*
 * Execute a jailed process using nsjail.
 *
 * config_pb/config_pb_len contain a fully-constructed protobuf
 * wire-format encoded nsjail.NsJailConfig.  The Rust caller is
 * responsible for building the complete config (base + overrides).
 *
 * Returns the exit code of the jailed process, or -1 on setup failure.
 */
int run_jail(const unsigned char* config_pb, size_t config_pb_len);

#ifdef __cplusplus
}
#endif

#endif /* RUN_JAIL_H_ */
