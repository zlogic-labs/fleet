import { Button, Popconfirm } from 'antd';
import { DeleteOutlined } from '@ant-design/icons';

/**
 * The delete control for a row that is itself clickable.
 *
 * It exists because getting this right takes three things at once: a
 * confirmation (a mistake here is an operator's project, not a row), a danger
 * treatment, and stopPropagation so choosing "delete" does not also count as
 * "select this row" and re-render the panel underneath. Every one of the admin
 * tables got that combination slightly differently, and two of them forgot the
 * stopPropagation.
 */
export function RowDelete<T>({
  row,
  title,
  description,
  onConfirm,
}: {
  row: T;
  title: string;
  description: string;
  onConfirm: (row: T) => void;
}) {
  return (
    <Popconfirm title={title} description={description} onConfirm={() => onConfirm(row)}>
      <Button
        type="text"
        danger
        size="small"
        icon={<DeleteOutlined />}
        onClick={(e) => e.stopPropagation()}
      />
    </Popconfirm>
  );
}
