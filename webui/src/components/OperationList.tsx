import React, { useEffect, useState } from "react";
import {
  Operation,
  OperationEvent,
  OperationEventType,
} from "../../gen/ts/v1/operations_pb";
import { Empty, List } from "antd";
import {
  BackupInfo,
  BackupInfoCollector,
  getOperations,
  subscribeToOperations,
  unsubscribeFromOperations,
} from "../state/oplog";
import _ from "lodash";
import { GetOperationsRequest } from "../../gen/ts/v1/service_pb";
import { useAlertApi } from "./Alerts";
import { OperationRow } from "./OperationRow";

// OperationList displays a list of operations that are either fetched based on 'req' or passed in via 'useBackups'.
// If showPlan is provided the planId will be displayed next to each operation in the operation list.
export const OperationList = ({
  req,
  useBackups,
  showPlan,
  filter,
}: React.PropsWithoutRef<{
  req?: GetOperationsRequest;
  useBackups?: BackupInfo[];
  showPlan?: boolean;
  filter?: (op: Operation) => boolean; // if provided, only operations that pass this filter will be displayed.
}>) => {
  const alertApi = useAlertApi();

  let backups: BackupInfo[] = [];
  if (req) {
    const [backupState, setBackups] = useState<BackupInfo[]>(useBackups || []);
    backups = backupState;

    // track backups for this operation tree view.
    useEffect(() => {
      if (!req) {
        return;
      }

      const backupCollector = new BackupInfoCollector(filter);
      const lis = (opEvent: OperationEvent) => {
        if (!!req.planId && opEvent.operation!.planId !== req.planId) {
          return;
        }
        if (!!req.repoId && opEvent.operation!.repoId !== req.repoId) {
          return;
        }
        if (opEvent.type !== OperationEventType.EVENT_DELETED) {
          backupCollector.addOperation(opEvent.type!, opEvent.operation!);
        } else {
          backupCollector.removeOperation(opEvent.operation!);
        }
      };
      subscribeToOperations(lis);

      backupCollector.subscribe(
        _.debounce(() => {
          let backups = backupCollector.getAll();
          backups.sort((a, b) => {
            return b.startTimeMs - a.startTimeMs;
          });
          setBackups(backups);
        }, 50),
      );

      getOperations(req)
        .then((ops) => {
          backupCollector.bulkAddOperations(ops);
        })
        .catch((e) => {
          alertApi!.error("Failed to fetch operations: " + e.message);
        });
      return () => {
        unsubscribeFromOperations(lis);
      };
    }, [JSON.stringify(req)]);
  } else {
    backups = [...(useBackups || [])];
  }

  if (backups.length === 0) {
    return (
      <Empty
        description="No operations yet."
        image={Empty.PRESENTED_IMAGE_SIMPLE}
      ></Empty>
    );
  }

  let operations = backups.flatMap((b) => b.operations);
  operations.sort((a, b) => {
    return Number(b.unixTimeStartMs - a.unixTimeStartMs);
  });
  return (
    <List
      itemLayout="horizontal"
      size="small"
      dataSource={operations}
      renderItem={(op) => {
        return (
          <OperationRow
            alertApi={alertApi!}
            key={op.id}
            operation={op}
            showPlan={showPlan || false}
          />
        );
      }}
      pagination={
        operations.length > 25
          ? { position: "both", align: "center", defaultPageSize: 25 }
          : undefined
      }
    />
  );
};
