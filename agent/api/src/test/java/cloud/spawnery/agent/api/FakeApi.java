/*
Copyright The Spawnery Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package cloud.spawnery.agent.api;

import java.util.List;
import java.util.Optional;
import java.util.UUID;

/** An implementation that answers nothing, for tests about the holder. */
final class FakeApi implements SpawneryApi {
    @Override
    public Self self() {
        return new ProxySelf() {
            @Override public String name() { return "gateway-0"; }
            @Override public String group() { return "gateway"; }
            @Override public String network() { return "production"; }
        };
    }

    @Override public List<Group> groups() { return List.of(); }
    @Override public Optional<Group> group(String name) { return Optional.empty(); }
    @Override public List<ServerInfo> servers() { return List.of(); }
    @Override public Optional<ServerInfo> server(String name) { return Optional.empty(); }
    @Override public List<CloudPlayer> players() { return List.of(); }
    @Override public Optional<CloudPlayer> player(UUID id) { return Optional.empty(); }
}
