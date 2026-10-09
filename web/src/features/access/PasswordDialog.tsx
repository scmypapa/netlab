import { Button, Modal, PasswordInput } from "@mantine/core";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";
import { api } from "../../api/client";
import { ErrorMessage } from "../../foundation/Feedback";

export function PasswordDialog({ onClose }: { onClose: () => void }) {
  const [currentPassword, setCurrent] = useState("");
  const [newPassword, setNew] = useState("");
  const client = useQueryClient();
  const save = useMutation({
    mutationFn: () => api.changePassword({ currentPassword, newPassword }),
    onSuccess: () => {
      client.clear();
      window.location.reload();
    },
  });
  return (
    <Modal opened onClose={onClose} title="修改密码" centered size="sm">
      <form
        className="form-stack"
        onSubmit={(event) => {
          event.preventDefault();
          save.mutate();
        }}
      >
        <PasswordInput
          label="当前密码"
          autoComplete="current-password"
          required
          autoFocus
          value={currentPassword}
          onChange={(event) => setCurrent(event.currentTarget.value)}
        />
        <PasswordInput
          label="新密码"
          autoComplete="new-password"
          required
          minLength={8}
          maxLength={72}
          value={newPassword}
          onChange={(event) => setNew(event.currentTarget.value)}
        />
        <ErrorMessage error={save.error} />
        <div className="dialog-actions">
          <Button variant="default" onClick={onClose}>
            取消
          </Button>
          <Button type="submit" loading={save.isPending}>
            保存并重新登录
          </Button>
        </div>
      </form>
    </Modal>
  );
}
