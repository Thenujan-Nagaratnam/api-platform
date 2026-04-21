Found more unused files: all 4 AsyncAPIPreview/ sub-files (ChannelCard, ChannelsSection, MessageCard, MessagesSection) have zero importers outside themselves — the folder is a dead island since we deleted the index and main file earlier.

Now I have the complete picture. Here's the properly thought-through plan:

First: Delete remaining dead files

views/DesignView/components/AsyncAPIPreview/ChannelCard.tsx       ← no importers
views/DesignView/components/AsyncAPIPreview/ChannelsSection.tsx   ← no importers
views/DesignView/components/AsyncAPIPreview/MessageCard.tsx       ← no importers
views/DesignView/components/AsyncAPIPreview/MessagesSection.tsx   ← no importers
views/DesignView/components/OperationsSection/components/index.ts ← barrel, no importers
views/DesignView/components/ComponentsSection/components/index.ts ← barrel, no importers
Proposed views/DesignView/components/ structure
The principle: group by OpenAPI concept — every file related to a concept lives in one folder. No more digging across spec/openapi/, editors/, and modals/ to find parameter-related files.


views/DesignView/
├── APIEditor.tsx
├── hooks/
│   ├── useAPIEditorState.ts
│   ├── useAPIEditorSpecLoader.ts
│   └── useAPIEditorActions.ts
└── components/
    │
    ├── api-header/                      ← top of the design view
    │   ├── APIHeader.tsx
    │   └── MetricsOverview.tsx          (moved from MetricsOverview/)
    │
    ├── api-info/                        ← spec metadata editing sections
    │   ├── BasicInfoSection.tsx
    │   ├── ServersSection.tsx
    │   └── SpecInfoEditor.tsx           (from components/spec/shared/ — used here alongside these two)
    │
    ├── operation/                       ← ALL operation-related in one place
    │   ├── OperationsSection.tsx        (from OperationsSection/OperationsSection.tsx)
    │   ├── OperationCard.tsx            (from OperationsSection/components/OperationCard.tsx)
    │   ├── OperationEditorModal.tsx     (from components/modals/OperationEditorModal/)
    │   └── tabs/
    │       ├── OverviewTab.tsx
    │       ├── RequestTab.tsx
    │       └── ResponseTab.tsx
    │
    ├── parameter/                       ← inline field + list + modal editor, all here
    │   ├── Parameter.tsx
    │   ├── ReadOnlyParameter.tsx
    │   ├── ParameterList.tsx            (renamed from Parameters/Parameters.tsx)
    │   └── ParameterEditor.tsx          (from components/editors/)
    │
    ├── header/
    │   ├── Header.tsx
    │   ├── ReadOnlyHeader.tsx
    │   ├── ReadOnlyHeaderList.tsx       (renamed from Headers/ReadOnlyHeaders.tsx)
    │   └── HeaderEditor.tsx             (from components/editors/)
    │
    ├── request-body/
    │   ├── RequestBody.tsx
    │   ├── RequestBodyReference.tsx     (flattened from RequestBody/components/)
    │   └── RequestBodyEditor.tsx        (from components/editors/)
    │
    ├── response/
    │   ├── Response.tsx
    │   ├── ReadOnlyResponse.tsx
    │   ├── ResponseList.tsx             (renamed from Responses/Responses.tsx)
    │   ├── ResponseAddMenu.tsx          (flattened from Responses/components/)
    │   └── ResponseEditor.tsx           (from components/editors/)
    │
    ├── media-type/                      ← inline media type + its examples, together
    │   ├── MediaType.tsx
    │   ├── ReadOnlyMediaType.tsx
    │   └── MediaTypeExamplesEditor.tsx  (renamed from ExampleEditor/ExampleEditor.tsx — inline examples for a media type)
    │
    ├── schema/
    │   ├── SchemaEditor.tsx
    │   ├── ReadOnlySchemaEditor.tsx
    │   └── SchemaEditorModal.tsx
    │
    ├── example/                         ← named example objects (components/examples section)
    │   └── ExampleObjectEditor.tsx      (renamed from editors/ExampleEditor.tsx — modal for named examples)
    │
    ├── security/
    │   └── SecuritySchemeEditor.tsx
    │
    ├── link/
    │   └── LinkEditor.tsx
    │
    ├── callback/
    │   └── CallbackEditor.tsx
    │
    ├── reference/                       ← $ref picker + inline reference components
    │   ├── RefComponent.tsx
    │   ├── ReferenceObject.tsx
    │   └── ReadOnlyReferenceObject.tsx
    │
    ├── components-section/              ← OpenAPI components panel (lists reusable objects)
    │   ├── ComponentsSection.tsx
    │   ├── ComponentItemCard.tsx        (from ComponentsSection/components/)
    │   ├── ComponentTypeGroup.tsx       (from ComponentsSection/components/)
    │   └── componentUtils.ts            (from ComponentsSection/utils/)
    │
    ├── openapi/                         ← OpenAPI editor entry point
    │   └── OpenAPIEditor.tsx
    │
    ├── asyncapi/                        ← ALL AsyncAPI related in one place
    │   ├── AsyncAPIEditor.tsx
    │   ├── AsyncAPIBasicInfoSection.tsx
    │   ├── AsyncAPIServersSection.tsx
    │   ├── AsyncAPIComponentsSection.tsx
    │   ├── MessagesEditorSection.tsx
    │   ├── ChannelsEditorSection.tsx
    │   ├── ChannelEditorModal.tsx       (from components/spec/asyncapi/)
    │   └── MessageEditorModal.tsx       (from components/spec/asyncapi/)
    │
    └── shared/                          ← shared styled components used across DesignView
        ├── EditorCommonStyles.tsx        (from components/spec/shared/)
        └── SpecSectionHeader.tsx         (renamed from spec/openapi/SectionHeader/SectionHeader.tsx)
What stays in components/ (genuinely cross-view shared)

components/
├── common/
│   ├── EntityModal.tsx        ← moved from modals/ (TestView also uses this)
│   ├── LoadingOverlay.tsx
│   ├── LoadingStates.tsx
│   ├── ViewContainer.tsx
│   ├── SpecTypeBadge.tsx
│   └── FeatureComingSoon.tsx
├── layout/                    ← used by ManageView sections
├── forms/                     ← used by ManageView sections
├── ai/
└── validation/
components/modals/ disappears entirely — EntityModal moves to common/, OperationEditorModal moves to operation/.

Does this look correct? Once confirmed I'll implement it — delete the dead files first, then do all moves + renames + import path updates.