# What exercises what

Each row is a change the fork makes, named as the scenarios name it in the
`covers` list of their `scenario.json`, with where it is in the source,
which scenarios and component modes exercise it against upstream, and what
else in the repository tests it. "Fuzz" means a native Go fuzz target in
the package, which compares with the library the code replaces rather than
with upstream.

The measured counterpart of this table is the coverage section of the
report: of the statements the fork changed in each file, how many the
differential runs executed. This table says what was intended; that says
what happened. Where they disagree, believe the report.

## State

| Change | Source | Scenarios | Component modes | Other tests |
|---|---|---|---|---|
| `state-read`: one-pass decoder for resources, shared dependency lists, cached address parsing | `states/statefile/version4.go`, `version4_decode_fast.go`, `read.go` | all; 16, 17, 20 most | `statefile` | Fuzz `UnmarshalResourceV4`, `ValidJSONValue`, `ParseSimpleStringArray`; round-trip tests |
| `state-decode-parallel`: resources split and decoded on several processors (256 kB and over), boundaries found concurrently (4 MB and over) | `states/statefile/version4_decode.go` | 16, 17 | `statefile` (every tenth input over 256 kB, every fortieth over 4 MB, including a layout built to mislead the boundary search) | `TestReadStateV4Parallel*`, `TestSplitJSONObjectArray` |
| `state-read-fallback`: anything unusual handed to the standard decoder | same | 20 | `statefile` (three of four layouts, and every damaged input) | |
| `state-write`: objects encoded concurrently in windows, attributes indented in one pass, dependencies written directly | `states/statefile/version4_encode.go`, `indent_json.go` | all | `statefile`, `values` | Fuzz `AppendIndentedJSON`; `TestEncodeStateV4*` |
| `state-encode-parallel`: the concurrent path, from 64 instance objects | same | 04, 16, 17, 18 | `statefile` | |
| `state-encode`: an object's serialized form remembered and reused while its value is the very same value; unchanged objects sharing bytes | `states/instance_object.go`, `instance_object_src.go`, `state_deepcopy.go` | all | `values` (encode, decode, encode again, and again after a state file) | `instance_object_encoded_test.go` |
| `value-json`: hand-written replacement for the value-to-JSON encoder | `states/marshal_json.go` | 16, 24 | `values`, `dyntypes` | Fuzz `MarshalJSONNumber`, `MarshalJSONString`; `marshal_json_test.go` |
| `value-json` (decoding): replacement for the JSON-to-value decoder | `lang/fastjson` | all; 16, 20 | `decode`, `values` | Fuzz `Unmarshal` |
| `value-msgpack`: replacement for the value-to-msgpack encoder | `lang/fastmsgpack` | all (every provider request, every plan) | `values` | Fuzz `Marshal` |
| `dynamic-types`: remembered serializations of types | `states/marshal_json.go`, `lang/fastjson` | 16 | `dyntypes`, `values` | |
| `state-upgrade`: skipping the removal of attributes when there are none to remove | `terraform/upgrade_resource_state.go`, `lang/fastjson` | 20 (has one to remove), all others (have none) | | `TestHasOnlySupportedAttributes` |
| `state-module-index`: index of the modules in a state, kept by the state's lock wrapper; read lock for the search for orphans | `states/state.go`, `states/sync.go`, `terraform/node_resource_plan.go` | 04, 05, 14, 19, 20 | | `sync_module_index_test.go` |
| `state-manager-local`: backup read once, file emptied only as the first bytes are written, comparison by digest | `states/statemgr/filesystem.go`, `statefile/marshal_equal.go` | all (backup compared at every step); 19 | | `filesystem_test.go` |
| `state-snapshots`: snapshots written in the background while applying a large state | `backend/local/hook_state.go`, `terraform/update_state_hook.go` | 17 (large, including a failing apply), all others (the unchanged path), 13, 15, 26 | interruption tier, both variants | `hook_state_test.go` |
| `state-manager-remote`: whether a remote state has changed decided from a digest; buffer sized from the last snapshot | `states/remote/state.go` | 26 | | `state_revision_test.go` |
| `check-results` | unchanged code, different order of work | 11 | | |
| `marks`: skipping work for values with no marks; recognizing that marking would reproduce the value already held | `lang/marks`, `terraform/node_resource_abstract_instance.go` | 10, 20 | `values` | `marks_test.go`, `paths_test.go` |
| `deposed`, `tainted` | unchanged code reached through changed encoders and indexes | 08, 09, 20, 23 | `statefile` | |

## Plans

| Change | Source | Scenarios | Component modes | Other tests |
|---|---|---|---|---|
| `plan-file`: members prepared concurrently, written without data descriptors, plan message with resource changes last | `plans/planfile/writer.go`, `tfplan.go` | all (each build reads the other's) | | `writer_test.go` |
| `plan-deflate`: members compressed in independent 1 MiB chunks | `plans/planfile/parallel_deflate.go` | 16, 17 (members over 1 MiB) | | `parallel_deflate_test.go` |
| `plan-snapshot-sharing`: the two state snapshots written once and read once when they're the same | `plans/planfile/writer.go`, `reader.go`, `statefile/marshal_equal.go` | every step where refreshing changes nothing; 24 where it mustn't apply; 12 where they differ | | `writer_test.go` |
| `changes-index`: planned changes found by address | `plans/changes_sync.go`, `changes_src.go` | all | `changes` | `changes_sync_test.go` |
| `output-index`: output changes found by address and by module call | `plans/changes_sync.go` | 03, 11, 18 | `changes` | `changes_sync_test.go` |
| `deferred-decode`: values of unchanged resources decoded only if something needs them | `plans/changes.go`, `changes_src.go`, `terraform/context_apply.go` | every apply of a saved plan with unchanged resources; 17 | | `changes_sync_test.go` |
| `plan-render`: diffs computed and rendered in parallel batches, nothing computed for unchanged resources | `command/jsonformat`, `command/jsonplan` | all (plan text compared exactly) | | |

## Graph

| Change | Source | Scenarios | Component modes | Other tests |
|---|---|---|---|---|
| `walker`: goroutines only for vertices that are ready, continuing inline | `dag/walk.go` | all; 15 and 23 for failures; 22 for order | `dag` | `walk_test.go` |
| `transitive-reduction`: indexed, concurrent, with reachability labels; on-stack set for cycle detection | `dag/transitive_reduction.go`, `indexed.go`, `tarjan.go` | all; 17, 18, 22 | `dag` | `TestAcyclicGraphTransitiveReduction_matchesGeneric` |
| `attach-dependencies`: dependencies of every node in one pass | `terraform/transform_reference.go`, `dag/indexed.go` | all (dependencies are in the state, which is compared) | | `node_resource_plan_instance_deps_test.go` |
| `module-expansion`: nodes connected to their module's expander through an index | `terraform/transform_module_expansion.go` | 03, 04, 13, 18, 21, 22, 25 | | |
| `destroy-edges`: the planned change for each creator found through an index | `terraform/transform_destroy_edge.go` | 08, 13, 22 | | |
| `data-depends-on`: reference map built only if there's a data resource | `terraform/transform_reference.go` | 07 (has them), most others (don't) | | |
| references resolved concurrently, edges added in bulk | `terraform/transform_reference.go`, `dag/graph.go` | all | | `TestGraphConnectAll` |

## Evaluation

| Change | Source | Scenarios | Component modes | Other tests |
|---|---|---|---|---|
| `resource-value-cache`: the value of a resource built once until the resource changes | `terraform/evaluate.go`, `states/sync.go`, `plans/changes_sync.go` | every scenario in which something refers to a resource; 02, 03, 14, 17, 18 most | | `value_known_test.go` |
| `for-each-cache`: a `for_each` expression evaluated once per module instance per walk | `terraform/eval_for_each.go` | 02, 04, 06, 16, 18, 25 (25 with JSON expressions) | | |
| `expander`: module instances found by key, and remembered | `instances/expander.go` | 02, 03, 04, 14, 18 | `expander` | |
| `named-values`: separate locks for variables, locals and outputs | `namedvals` | 03, 11, 18 | | race detector |
| `function-tables`: function tables shared between the scopes of a module | `lang/functions.go`, `lang/function_results.go` | 03, 18, 25 | | |
| `refresh-unchanged`: an object that refresh or plan returns unchanged keeps the value it had | `terraform/node_resource_abstract_instance.go` | every step that refreshes; 10 with marks; 12 where it does change | | |
| `targeting` | unchanged code, reached through the changed cache and expander | 14 | | |
| `moves`: move statements compared concurrently; implied moves through the module index | `refactoring` | 05 | | upstream's tests |
| `import`, `refresh-only`, `failure`, `apply-without-plan-file` | unchanged code reached through changed code | 06; 12; 09, 15, 23; 13 | | |

## Settings only the fork has

| Change | Source | Scenarios | Other tests |
|---|---|---|---|
| `skip-validate`: `TF_SKIP_VALIDATE` | `backend/local/backend_local.go` | 21 | |
| `TF_GCS_STATE_GZIP` | `backend/remote-state/gcs` | none | `client_upload_test.go` |
| `TF_PROFILE_*` | `profile.go`, `logging/phases*.go` | none | none |

## Not exercised by the proof at all

These need a service or an emulator, which the proof doesn't have. They are
counted in the coverage figure, as uncovered:

| Change | Source | What tests it |
|---|---|---|
| GCS state: single-request upload with retries, skipping a download when the generation is unchanged, ranged download, optional gzip | `backend/remote-state/gcs/client.go`, and the part of `states/remote/state.go` that asks a client for its revision | Unit tests against in-process HTTP doubles: `client_upload_test.go`, `client_download_test.go`, `client_download_audit_test.go`, `state_revision_test.go` |
| GCS modules: shared client, concurrent download, prefetching | `getmodules/gcs_getter.go`, `getmodules/installer.go`, `initwd/module_install.go` | `gcs_getter_test.go`, against a double |

Putting the GCS backend behind an emulator, and running the scenarios with
their state in it, is the largest single thing that would raise both the
coverage figure and what the proof says about the configuration the fork
was written for.
