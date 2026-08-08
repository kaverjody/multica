"use client";

import { useMemo, useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { ChevronDown, UserPlus, Users, X } from "lucide-react";
import { api, ApiError } from "@multica/core/api";
import { useAuthStore } from "@multica/core/auth";
import { useWorkspaceId } from "@multica/core/hooks";
import { useWorkspacePaths } from "@multica/core/paths";
import {
  agentListOptions,
  memberListOptions,
  squadListOptions,
  workspaceKeys,
} from "@multica/core/workspace/queries";
import { AGENT_DESCRIPTION_MAX_LENGTH } from "@multica/core/agents";
import { isImeComposing } from "@multica/core/utils";
import type { Agent, MemberWithUser, Squad } from "@multica/core/types";
import {
  Dialog,
  DialogContent,
  DialogHeader,
  DialogTitle,
  DialogDescription,
} from "@multica/ui/components/ui/dialog";
import {
  Popover,
  PopoverContent,
  PopoverTrigger,
} from "@multica/ui/components/ui/popover";
import { Button } from "@multica/ui/components/ui/button";
import { Input } from "@multica/ui/components/ui/input";
import { Label } from "@multica/ui/components/ui/label";
import { toast } from "sonner";

import { useNavigation } from "../navigation";
import { ActorAvatar } from "../common/actor-avatar";
import { AvatarUploadControl } from "../common/avatar-upload-control";
import { CharCounter } from "../agents/components/char-counter";
import {
  PickerEmpty,
  PickerItem,
  PickerSection,
} from "../issues/components/pickers/property-picker";
import { matchesPinyin } from "../editor/extensions/pinyin-match";
import { useT } from "../i18n";

type SelectedMember = {
  type: "agent" | "member";
  id: string;
  name: string;
  // Optional role shown in the create form and sent with the atomic create
  // (F2, LIU-9). Mirrors the detail page AddMemberDialog role input.
  role?: string;
};

type SelectedSquad = {
  id: string;
  name: string;
};

// How many chips we show inline before collapsing the tail into "+N".
const CHIP_DISPLAY_LIMIT = 3;

// B07 (LIU-9 子任务A): the atomic create endpoint rejects the whole request
// with `{ error, failed_members: [{member_type, member_id, reason}] }` when any
// member is invalid. This extracts the failed entries so the form can name
// exactly which selections to fix.
type FailedMember = { member_type: string; member_id: string; reason: string };

function extractFailedMembers(err: unknown): FailedMember[] {
  if (err instanceof ApiError && typeof err.body === "object" && err.body !== null) {
    const body = err.body as { failed_members?: unknown };
    if (!Array.isArray(body.failed_members)) return [];
    return body.failed_members.filter(
      (f): f is FailedMember =>
        typeof f === "object" &&
        f !== null &&
        typeof (f as FailedMember).member_type === "string" &&
        typeof (f as FailedMember).member_id === "string" &&
        typeof (f as FailedMember).reason === "string",
    );
  }
  return [];
}

export function CreateSquadModal({ onClose }: { onClose: () => void }) {
  const { t } = useT("modals");
  const router = useNavigation();
  const wsPaths = useWorkspacePaths();
  const wsId = useWorkspaceId();
  const queryClient = useQueryClient();
  const currentUser = useAuthStore((s) => s.user);
  const currentUserId = currentUser?.id ?? null;

  const { data: agents = [] } = useQuery(agentListOptions(wsId));
  const { data: wsMembers = [] } = useQuery(memberListOptions(wsId));
  const { data: squads = [] } = useQuery(squadListOptions(wsId));

  const activeAgents = useMemo(
    () => agents.filter((a: Agent) => !a.archived_at && a.runtime_id),
    [agents],
  );

  // Squads eligible to be merged into the new one: the list endpoint only
  // returns unarchived squads, and v1 nesting is one level, so squads that
  // already belong to another parent are excluded.
  const includableSquads = useMemo(
    () => squads.filter((s: Squad) => !s.parent_squad_id),
    [squads],
  );

  const [name, setName] = useState("");
  const [description, setDescription] = useState("");
  const [avatarUrl, setAvatarUrl] = useState<string | null>(null);
  const [leaderId, setLeaderId] = useState("");
  const [selectedMembers, setSelectedMembers] = useState<SelectedMember[]>([]);
  const [selectedSquads, setSelectedSquads] = useState<SelectedSquad[]>([]);
  const [creating, setCreating] = useState(false);

  // Promoting an agent to leader must actually drop it from selectedMembers,
  // not merely hide it. Otherwise switching leader away later resurrects the
  // hidden pick and silently submits it as a member.
  const handleLeaderChange = (id: string) => {
    setLeaderId(id);
    if (id) {
      setSelectedMembers((prev) =>
        prev.filter((m) => !(m.type === "agent" && m.id === id)),
      );
    }
  };

  const canSubmit = !!name.trim() && !!leaderId && !creating;

  const handleSubmit = async () => {
    if (!canSubmit) return;
    setCreating(true);
    try {
      // F1 (LIU-9 子任务A): one-shot atomic create — squad + members in a
      // single request. The old two-step "create then add members" flow is
      // gone: any invalid member fails the whole create and the response
      // carries failed_members, which we surface so the user can fix the
      // rejected entries and resubmit.
      const squad = await api.createSquad({
        name: name.trim(),
        description: description.trim() || undefined,
        leader_id: leaderId,
        avatar_url: avatarUrl ?? undefined,
        // Only send the field when squads were actually picked; an empty
        // selection omits it so the payload stays minimal.
        included_squad_ids:
          selectedSquads.length > 0
            ? selectedSquads.map((s) => s.id)
            : undefined,
        members:
          selectedMembers.length > 0
            ? selectedMembers.map((m) => ({
                member_type: m.type,
                member_id: m.id,
                // CLO-418: the backend requires a role for every member; the
                // UI default is "member" when the user left it blank.
                role: m.role?.trim() || "member",
              }))
            : undefined,
      });
      queryClient.invalidateQueries({ queryKey: workspaceKeys.squads(wsId) });
      queryClient.invalidateQueries({
        queryKey: [...workspaceKeys.squads(wsId), squad.id, "members"],
      });

      onClose();
      toast.success(t(($) => $.create_squad.toast_created));
      router.push(wsPaths.squadDetail(squad.id));
    } catch (err) {
      // B07 (LIU-9 子任务A): the server rejects the whole create with
      // { error, failed_members: [{member_type, member_id, reason}] } when any
      // member is invalid. Name the rejected entries instead of a generic
      // failure so the user can see which selection to fix.
      const failed = extractFailedMembers(err);
      if (failed.length > 0) {
        const names = failed
          .map((f) => {
            const known = selectedMembers.find(
              (m) => m.type === f.member_type && m.id === f.member_id,
            );
            return known ? known.name : f.member_id;
          })
          .join(", ");
        toast.error(t(($) => $.create_squad.toast_members_failed, { names }));
      } else {
        toast.error(
          err instanceof Error
            ? err.message
            : t(($) => $.create_squad.toast_failed),
        );
      }
      setCreating(false);
    }
  };

  return (
    <Dialog open onOpenChange={(v) => { if (!v) onClose(); }}>
      <DialogContent className="p-0 gap-0 flex flex-col overflow-hidden !top-1/2 !left-1/2 !-translate-x-1/2 !-translate-y-1/2 !w-full !max-w-2xl !h-[85vh]">
        <DialogHeader className="border-b px-5 py-3 space-y-0">
          <DialogTitle className="text-base font-semibold">
            {t(($) => $.create_squad.title)}
          </DialogTitle>
          <DialogDescription className="mt-1 text-xs">
            {t(($) => $.create_squad.description)}
          </DialogDescription>
        </DialogHeader>

        <div className="flex-1 overflow-y-auto p-5">
          <div className="space-y-4 min-w-0">
            {/* Identity row mirrors CreateAgentDialog so the two creates read
                as siblings — avatar (left) + name/description stack (right). */}
            <div className="flex items-start gap-4">
              <AvatarUploadControl
                variant="squad"
                value={avatarUrl}
                name={name}
                size={64}
                onUploaded={setAvatarUrl}
                onClear={() => setAvatarUrl(null)}
              />
              <div className="flex-1 min-w-0 space-y-3">
                <div>
                  <Label className="text-xs text-muted-foreground">
                    {t(($) => $.create_squad.name_label)}
                  </Label>
                  <Input
                    autoFocus
                    type="text"
                    value={name}
                    onChange={(e) => setName(e.target.value)}
                    placeholder={t(($) => $.create_squad.name_placeholder)}
                    className="mt-1"
                    onKeyDown={(e) => {
                      if (isImeComposing(e)) return;
                      if (e.key === "Enter") void handleSubmit();
                    }}
                  />
                </div>

                <div>
                  <Label className="text-xs text-muted-foreground">
                    {t(($) => $.create_squad.description_label)}
                  </Label>
                  <Input
                    type="text"
                    value={description}
                    onChange={(e) => setDescription(e.target.value)}
                    placeholder={t(($) => $.create_squad.description_placeholder)}
                    maxLength={AGENT_DESCRIPTION_MAX_LENGTH}
                    className="mt-1"
                  />
                  <div className="mt-1">
                    <CharCounter
                      length={[...description].length}
                      max={AGENT_DESCRIPTION_MAX_LENGTH}
                    />
                  </div>
                </div>
              </div>
            </div>

            <LeaderPicker
              agents={activeAgents}
              currentUserId={currentUserId}
              value={leaderId}
              onChange={handleLeaderChange}
            />

            <IncludedSquadsPicker
              squads={includableSquads}
              value={selectedSquads}
              onChange={setSelectedSquads}
            />

            <AdditionalMembersPicker
              agents={activeAgents}
              members={wsMembers}
              currentUserId={currentUserId}
              leaderId={leaderId}
              value={selectedMembers}
              onChange={setSelectedMembers}
            />
          </div>
        </div>

        {/* Inline footer — see CreateAgentDialog: shadcn DialogFooter applies
            negative margins assuming a padded DialogContent. Our content is
            p-0, so a plain bordered row is the right call. */}
        <div className="flex items-center justify-end gap-2 border-t bg-background px-5 py-3">
          <Button variant="ghost" onClick={onClose}>
            {t(($) => $.create_squad.cancel)}
          </Button>
          <Button onClick={() => void handleSubmit()} disabled={!canSubmit}>
            {creating
              ? t(($) => $.create_squad.submitting)
              : t(($) => $.create_squad.submit)}
          </Button>
        </div>
      </DialogContent>
    </Dialog>
  );
}

// ---------------------------------------------------------------------------
// LeaderPicker — single-select agent picker, grouped "My Agents" first then
// "Workspace Agents". Empty workspaces render a disabled trigger that points
// the user at agent creation.
// ---------------------------------------------------------------------------
function LeaderPicker({
  agents,
  currentUserId,
  value,
  onChange,
}: {
  agents: Agent[];
  currentUserId: string | null;
  value: string;
  onChange: (id: string) => void;
}) {
  const { t } = useT("modals");
  const [open, setOpen] = useState(false);
  const [filter, setFilter] = useState("");

  const myAgents = useMemo(
    () => (currentUserId ? agents.filter((a) => a.owner_id === currentUserId) : []),
    [agents, currentUserId],
  );
  const otherAgents = useMemo(
    () =>
      currentUserId
        ? agents.filter((a) => a.owner_id !== currentUserId)
        : agents,
    [agents, currentUserId],
  );

  const q = filter.trim().toLowerCase();
  const matches = (a: Agent) =>
    !q || a.name.toLowerCase().includes(q) || matchesPinyin(a.name, q);
  const filteredMine = myAgents.filter(matches);
  const filteredOthers = otherAgents.filter(matches);

  const selected = agents.find((a) => a.id === value) ?? null;
  const noAgents = agents.length === 0;

  return (
    <div>
      <Label className="text-xs text-muted-foreground">
        {t(($) => $.create_squad.leader_label)}
      </Label>
      <p className="mt-0.5 mb-1.5 text-xs text-muted-foreground">
        {t(($) => $.create_squad.leader_hint)}
      </p>

      {noAgents ? (
        <div className="flex items-center gap-2 rounded-lg border border-dashed bg-muted/30 px-3 py-2.5 text-sm text-muted-foreground">
          {t(($) => $.create_squad.no_agents)}
        </div>
      ) : (
        <Popover
          open={open}
          onOpenChange={(v) => {
            setOpen(v);
            if (!v) setFilter("");
          }}
        >
          <PopoverTrigger className="flex w-full min-w-0 items-center gap-3 rounded-lg border border-border bg-background px-3 py-2.5 text-left text-sm transition-colors hover:bg-muted">
            {selected ? (
              <ActorAvatar actorType="agent" actorId={selected.id} size="sm" showStatusDot />
            ) : (
              <UserPlus className="h-4 w-4 shrink-0 text-muted-foreground" />
            )}
            <div className="min-w-0 flex-1">
              <div className="truncate font-medium">
                {selected?.name ?? t(($) => $.create_squad.leader_placeholder)}
              </div>
              {selected?.description && (
                <div className="truncate text-xs text-muted-foreground">
                  {selected.description}
                </div>
              )}
            </div>
            <ChevronDown
              className={`h-4 w-4 shrink-0 text-muted-foreground transition-transform ${
                open ? "rotate-180" : ""
              }`}
            />
          </PopoverTrigger>
          <PopoverContent align="start" className="w-[var(--anchor-width)] p-0">
            <div className="border-b px-2 py-1.5">
              <input
                autoFocus
                type="text"
                value={filter}
                onChange={(e) => setFilter(e.target.value)}
                placeholder={t(($) => $.create_squad.picker_search_placeholder)}
                className="w-full bg-transparent text-sm placeholder:text-muted-foreground outline-none"
              />
            </div>
            <div className="max-h-72 overflow-y-auto p-1">
              {filteredMine.length > 0 && (
                <PickerSection label={t(($) => $.create_squad.group_my_agents)}>
                  {filteredMine.map((a) => (
                    <PickerItem
                      key={a.id}
                      selected={value === a.id}
                      onClick={() => {
                        onChange(a.id);
                        setOpen(false);
                        setFilter("");
                      }}
                    >
                      <ActorAvatar actorType="agent" actorId={a.id} size="sm" showStatusDot />
                      <span className="truncate">{a.name}</span>
                    </PickerItem>
                  ))}
                </PickerSection>
              )}
              {filteredOthers.length > 0 && (
                <PickerSection label={t(($) => $.create_squad.group_workspace_agents)}>
                  {filteredOthers.map((a) => (
                    <PickerItem
                      key={a.id}
                      selected={value === a.id}
                      onClick={() => {
                        onChange(a.id);
                        setOpen(false);
                        setFilter("");
                      }}
                    >
                      <ActorAvatar actorType="agent" actorId={a.id} size="sm" showStatusDot />
                      <span className="truncate">{a.name}</span>
                    </PickerItem>
                  ))}
                </PickerSection>
              )}
              {filteredMine.length === 0 && filteredOthers.length === 0 && (
                <PickerEmpty />
              )}
            </div>
          </PopoverContent>
        </Popover>
      )}
    </div>
  );
}

// ---------------------------------------------------------------------------
// AdditionalMembersPicker — multi-select agents + workspace members. The
// trigger shows up to 3 chips inline; the rest collapse into "+N". The popup
// stays open while the user toggles selections so they can pick multiple
// without re-opening it.
// ---------------------------------------------------------------------------
function AdditionalMembersPicker({
  agents,
  members,
  currentUserId,
  leaderId,
  value,
  onChange,
}: {
  agents: Agent[];
  members: MemberWithUser[];
  currentUserId: string | null;
  leaderId: string;
  value: SelectedMember[];
  onChange: (next: SelectedMember[]) => void;
}) {
  const { t } = useT("modals");
  const [open, setOpen] = useState(false);
  const [filter, setFilter] = useState("");

  const isSelected = (type: "agent" | "member", id: string) =>
    value.some((m) => m.type === type && m.id === id);

  const toggle = (m: SelectedMember) => {
    if (isSelected(m.type, m.id)) {
      onChange(value.filter((x) => !(x.type === m.type && x.id === m.id)));
    } else {
      onChange([...value, m]);
    }
  };

  const remove = (m: SelectedMember) => {
    onChange(value.filter((x) => !(x.type === m.type && x.id === m.id)));
  };

  // F2 (LIU-9 子任务A): each selected member can carry an optional role,
  // mirroring the detail page AddMemberDialog role input.
  const setRole = (m: SelectedMember, role: string) => {
    onChange(
      value.map((x) =>
        x.type === m.type && x.id === m.id ? { ...x, role } : x,
      ),
    );
  };

  const myAgents = useMemo(
    () =>
      currentUserId
        ? agents.filter((a) => a.owner_id === currentUserId && a.id !== leaderId)
        : [],
    [agents, currentUserId, leaderId],
  );
  const otherAgents = useMemo(
    () =>
      agents.filter(
        (a) =>
          a.id !== leaderId &&
          (currentUserId ? a.owner_id !== currentUserId : true),
      ),
    [agents, currentUserId, leaderId],
  );

  const q = filter.trim().toLowerCase();
  const agentMatches = (a: Agent) =>
    !q || a.name.toLowerCase().includes(q) || matchesPinyin(a.name, q);
  const memberMatches = (m: MemberWithUser) =>
    !q || m.name.toLowerCase().includes(q) || matchesPinyin(m.name, q);

  const filteredMine = myAgents.filter(agentMatches);
  const filteredOthers = otherAgents.filter(agentMatches);
  const filteredMembers = members.filter(memberMatches);
  const anyResults =
    filteredMine.length + filteredOthers.length + filteredMembers.length > 0;

  return (
    <div>
      <Label className="text-xs text-muted-foreground">
        {t(($) => $.create_squad.members_label)}{" "}
        <span className="text-muted-foreground/60">
          {t(($) => $.create_squad.members_optional)}
        </span>
      </Label>
      <p className="mt-0.5 mb-1.5 text-xs text-muted-foreground">
        {t(($) => $.create_squad.members_hint)}
      </p>

      <Popover
        open={open}
        onOpenChange={(v) => {
          setOpen(v);
          if (!v) setFilter("");
        }}
      >
        {/* render={<div role="combobox" />} — chips contain their own remove
            <button>, so the trigger cannot itself be a <button> without
            nesting interactive content. Base UI injects click/keyboard/ARIA
            wiring into the rendered element. */}
        <PopoverTrigger
          render={
            <div
              role="combobox"
              aria-haspopup="listbox"
              aria-expanded={open}
              aria-controls="squad-member-listbox"
              tabIndex={0}
              className="flex w-full min-w-0 cursor-pointer items-center gap-3 rounded-lg border border-border bg-background px-3 py-2.5 text-left text-sm transition-colors hover:bg-muted focus:outline-hidden focus-visible:ring-2 focus-visible:ring-ring"
            >
              {value.length === 0 ? (
                <>
                  <UserPlus className="h-4 w-4 shrink-0 text-muted-foreground" />
                  <span className="min-w-0 flex-1 truncate text-muted-foreground">
                    {t(($) => $.create_squad.members_placeholder)}
                  </span>
                </>
              ) : (
                <div className="flex min-w-0 flex-1 flex-wrap items-center gap-1.5">
                  {value.slice(0, CHIP_DISPLAY_LIMIT).map((m) => (
                    <MemberChip
                      key={`${m.type}:${m.id}`}
                      m={m}
                      onRemove={() => remove(m)}
                    />
                  ))}
                  {value.length > CHIP_DISPLAY_LIMIT && (
                    <span className="rounded-md bg-muted px-1.5 py-0.5 text-xs font-medium text-muted-foreground tabular-nums">
                      {t(($) => $.create_squad.members_more_count, {
                        count: value.length - CHIP_DISPLAY_LIMIT,
                      })}
                    </span>
                  )}
                </div>
              )}
              <ChevronDown
                className={`h-4 w-4 shrink-0 text-muted-foreground transition-transform ${
                  open ? "rotate-180" : ""
                }`}
              />
            </div>
          }
        />
        <PopoverContent align="start" className="w-[var(--anchor-width)] p-0">
          <div className="border-b px-2 py-1.5">
            <input
              autoFocus
              type="text"
              value={filter}
              onChange={(e) => setFilter(e.target.value)}
              placeholder={t(($) => $.create_squad.picker_search_placeholder)}
              className="w-full bg-transparent text-sm placeholder:text-muted-foreground outline-none"
            />
          </div>
          <div id="squad-member-listbox" role="listbox" className="max-h-72 overflow-y-auto p-1">
            {filteredMine.length > 0 && (
              <PickerSection label={t(($) => $.create_squad.group_my_agents)}>
                {filteredMine.map((a) => (
                  <PickerItem
                    key={a.id}
                    selected={isSelected("agent", a.id)}
                    onClick={() =>
                      toggle({ type: "agent", id: a.id, name: a.name })
                    }
                  >
                    <ActorAvatar actorType="agent" actorId={a.id} size="sm" showStatusDot />
                    <span className="truncate">{a.name}</span>
                  </PickerItem>
                ))}
              </PickerSection>
            )}
            {filteredOthers.length > 0 && (
              <PickerSection label={t(($) => $.create_squad.group_workspace_agents)}>
                {filteredOthers.map((a) => (
                  <PickerItem
                    key={a.id}
                    selected={isSelected("agent", a.id)}
                    onClick={() =>
                      toggle({ type: "agent", id: a.id, name: a.name })
                    }
                  >
                    <ActorAvatar actorType="agent" actorId={a.id} size="sm" showStatusDot />
                    <span className="truncate">{a.name}</span>
                  </PickerItem>
                ))}
              </PickerSection>
            )}
            {filteredMembers.length > 0 && (
              <PickerSection label={t(($) => $.create_squad.group_members)}>
                {filteredMembers.map((m) => (
                  <PickerItem
                    key={m.user_id}
                    selected={isSelected("member", m.user_id)}
                    onClick={() =>
                      toggle({ type: "member", id: m.user_id, name: m.name })
                    }
                  >
                    <ActorAvatar actorType="member" actorId={m.user_id} size="sm" />
                    <span className="truncate">{m.name}</span>
                  </PickerItem>
                ))}
              </PickerSection>
            )}
            {!anyResults && <PickerEmpty />}
          </div>
        </PopoverContent>
      </Popover>

      {/* Selected members with an optional role each (F2, LIU-9). Rendered
          under the trigger so the user can fill roles without keeping the
          popover open. */}
      {value.length > 0 && (
        <div className="mt-2 space-y-1.5">
          {value.map((m) => (
            <div
              key={`${m.type}:${m.id}`}
              className="flex items-center gap-2 rounded-lg border border-border bg-background px-2.5 py-1.5"
            >
              <ActorAvatar actorType={m.type} actorId={m.id} size="xs" />
              <span className="min-w-0 flex-1 truncate text-sm">{m.name}</span>
              <Input
                type="text"
                value={m.role ?? ""}
                onChange={(e) => setRole(m, e.target.value)}
                placeholder={t(($) => $.create_squad.member_role_placeholder)}
                aria-label={t(($) => $.create_squad.member_role_label, {
                  name: m.name,
                })}
                className="h-7 w-40 text-xs"
              />
              <button
                type="button"
                onClick={() => remove(m)}
                aria-label={t(($) => $.create_squad.members_remove_aria, {
                  name: m.name,
                })}
                className="shrink-0 rounded-full text-muted-foreground hover:text-foreground"
              >
                <X className="h-3.5 w-3.5" />
              </button>
            </div>
          ))}
          <p className="text-xs text-muted-foreground">
            {t(($) => $.create_squad.member_role_optional)}
          </p>
        </div>
      )}
    </div>
  );
}

// ---------------------------------------------------------------------------
// IncludedSquadsPicker — multi-select of squads to merge under the new squad
// (LIU-8 squad nesting). Only unarchived, not-yet-nested squads are offered.
// Uses the same chip + searchable popover interaction as AdditionalMembersPicker.
// ---------------------------------------------------------------------------
function IncludedSquadsPicker({
  squads,
  value,
  onChange,
}: {
  squads: Squad[];
  value: SelectedSquad[];
  onChange: (next: SelectedSquad[]) => void;
}) {
  const { t } = useT("modals");
  const [open, setOpen] = useState(false);
  const [filter, setFilter] = useState("");

  const isSelected = (id: string) => value.some((s) => s.id === id);

  const toggle = (s: SelectedSquad) => {
    if (isSelected(s.id)) {
      onChange(value.filter((x) => x.id !== s.id));
    } else {
      onChange([...value, s]);
    }
  };

  const remove = (s: SelectedSquad) => {
    onChange(value.filter((x) => x.id !== s.id));
  };

  const q = filter.trim().toLowerCase();
  const matches = (s: Squad) =>
    !q || s.name.toLowerCase().includes(q) || matchesPinyin(s.name, q);
  const filtered = squads.filter(matches);

  return (
    <div>
      <Label className="text-xs text-muted-foreground">
        {t(($) => $.create_squad.include_squads_label)}{" "}
        <span className="text-muted-foreground/60">
          {t(($) => $.create_squad.include_squads_optional)}
        </span>
      </Label>
      <p className="mt-0.5 mb-1.5 text-xs text-muted-foreground">
        {t(($) => $.create_squad.include_squads_hint)}
      </p>

      <Popover
        open={open}
        onOpenChange={(v) => {
          setOpen(v);
          if (!v) setFilter("");
        }}
      >
        <PopoverTrigger
          render={
            <div
              role="combobox"
              aria-haspopup="listbox"
              aria-expanded={open}
              aria-controls="squad-include-listbox"
              tabIndex={0}
              className="flex w-full min-w-0 cursor-pointer items-center gap-3 rounded-lg border border-border bg-background px-3 py-2.5 text-left text-sm transition-colors hover:bg-muted focus:outline-hidden focus-visible:ring-2 focus-visible:ring-ring"
            >
              {value.length === 0 ? (
                <>
                  <Users className="h-4 w-4 shrink-0 text-muted-foreground" />
                  <span className="min-w-0 flex-1 truncate text-muted-foreground">
                    {t(($) => $.create_squad.include_squads_placeholder)}
                  </span>
                </>
              ) : (
                <div className="flex min-w-0 flex-1 flex-wrap items-center gap-1.5">
                  {value.slice(0, CHIP_DISPLAY_LIMIT).map((s) => (
                    <SquadChip
                      key={s.id}
                      s={s}
                      onRemove={() => remove(s)}
                    />
                  ))}
                  {value.length > CHIP_DISPLAY_LIMIT && (
                    <span className="rounded-md bg-muted px-1.5 py-0.5 text-xs font-medium text-muted-foreground tabular-nums">
                      {t(($) => $.create_squad.members_more_count, {
                        count: value.length - CHIP_DISPLAY_LIMIT,
                      })}
                    </span>
                  )}
                </div>
              )}
              <ChevronDown
                className={`h-4 w-4 shrink-0 text-muted-foreground transition-transform ${
                  open ? "rotate-180" : ""
                }`}
              />
            </div>
          }
        />
        <PopoverContent align="start" className="w-[var(--anchor-width)] p-0">
          <div className="border-b px-2 py-1.5">
            <input
              autoFocus
              type="text"
              value={filter}
              onChange={(e) => setFilter(e.target.value)}
              placeholder={t(($) => $.create_squad.picker_search_placeholder)}
              className="w-full bg-transparent text-sm placeholder:text-muted-foreground outline-none"
            />
          </div>
          <div id="squad-include-listbox" role="listbox" className="max-h-72 overflow-y-auto p-1">
            {filtered.length > 0 ? (
              <PickerSection label={t(($) => $.create_squad.group_squads)}>
                {filtered.map((s) => (
                  <PickerItem
                    key={s.id}
                    selected={isSelected(s.id)}
                    onClick={() => toggle({ id: s.id, name: s.name })}
                  >
                    <Users className="h-4 w-4 shrink-0 text-muted-foreground" />
                    <span className="truncate">{s.name}</span>
                    <span className="ml-auto shrink-0 text-xs text-muted-foreground tabular-nums">
                      {s.member_count ?? 0}
                    </span>
                  </PickerItem>
                ))}
              </PickerSection>
            ) : (
              <PickerEmpty />
            )}
          </div>
        </PopoverContent>
      </Popover>
    </div>
  );
}

function SquadChip({
  s,
  onRemove,
}: {
  s: SelectedSquad;
  onRemove: () => void;
}) {
  const { t } = useT("modals");
  return (
    <span className="inline-flex items-center gap-1 rounded-full border bg-background px-1.5 py-0.5 text-xs">
      <Users className="h-3 w-3 shrink-0 text-muted-foreground" />
      <span className="max-w-[120px] truncate">{s.name}</span>
      <button
        type="button"
        onClick={(e) => {
          e.preventDefault();
          e.stopPropagation();
          onRemove();
        }}
        aria-label={t(($) => $.create_squad.members_remove_aria, { name: s.name })}
        className="rounded-full text-muted-foreground hover:text-foreground"
      >
        <X className="h-3 w-3" />
      </button>
    </span>
  );
}

function MemberChip({
  m,
  onRemove,
}: {
  m: SelectedMember;
  onRemove: () => void;
}) {
  const { t } = useT("modals");
  return (
    <span className="inline-flex items-center gap-1 rounded-full border bg-background px-1.5 py-0.5 text-xs">
      <ActorAvatar actorType={m.type} actorId={m.id} size="xs" />
      <span className="max-w-[120px] truncate">{m.name}</span>
      <button
        type="button"
        onClick={(e) => {
          e.preventDefault();
          e.stopPropagation();
          onRemove();
        }}
        aria-label={t(($) => $.create_squad.members_remove_aria, { name: m.name })}
        className="rounded-full text-muted-foreground hover:text-foreground"
      >
        <X className="h-3 w-3" />
      </button>
    </span>
  );
}
